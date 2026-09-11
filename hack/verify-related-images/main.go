// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// verify-related-images validates image metadata in a final, built bundle CSV.
// It deliberately does not compare bundle image references to development manifests.
package main

import (
	_ "crypto/sha256" // Register SHA-256 for go-digest validation.
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"github.com/openshift-hyperfleet/hyperfleet-operator/internal/component/api"
	"sigs.k8s.io/yaml"
)

const relatedImagePrefix = "RELATED_IMAGE_"

type imageEntry struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type csvDocument struct {
	Spec struct {
		Install struct {
			Spec struct {
				Deployments []struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []container `json:"containers"`
							} `json:"spec"`
						} `json:"template"`
					} `json:"spec"`
				} `json:"deployments"`
			} `json:"spec"`
		} `json:"install"`
		RelatedImages []imageEntry `json:"relatedImages"`
	} `json:"spec"`
}

type container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Env   []env  `json:"env"`
}

type env struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func main() {
	csvPath := flag.String("csv", "", "required: CSV extracted from the built bundle")
	flag.Parse()
	if *csvPath == "" {
		fmt.Fprintln(os.Stderr,
			"provide -csv pointing to the final built bundle CSV; development manifests are not bundle inputs")
		os.Exit(1)
	}
	data, err := os.ReadFile(*csvPath)
	if err == nil {
		err = verifyCSV(data)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "related image verification failed:\n%v\n", err)
		os.Exit(1)
	}
	fmt.Println("bundle related image verification passed")
}

func verifyCSV(data []byte) error {
	var csv csvDocument
	if err := yaml.Unmarshal(data, &csv); err != nil {
		return fmt.Errorf("parse CSV YAML: %w", err)
	}
	images, problems := deployableImages(csv)
	// Require the same override consumed by the runtime. Absence is an error
	// even if relatedImages also omits the API: otherwise runtime uses a fallback
	// which the bundle mirroring metadata does not describe.
	apiName := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(api.RelatedImageEnv, relatedImagePrefix), "_", "-"))
	if !slices.ContainsFunc(images, func(entry imageEntry) bool { return entry.Name == apiName }) {
		problems = append(problems, "CSV manager is missing required runtime override "+api.RelatedImageEnv)
	}
	problems = append(problems,
		compareImages("CSV image sources", images, "CSV spec.relatedImages", csv.Spec.RelatedImages)...)
	return problemError(problems)
}

func deployableImages(csv csvDocument) ([]imageEntry, []string) {
	var managerImages []string
	var images []imageEntry
	problems := []string{}
	for _, deployment := range csv.Spec.Install.Spec.Deployments {
		for _, c := range deployment.Spec.Template.Spec.Containers {
			if c.Name == "manager" {
				managerImages = append(managerImages, c.Image)
			}
			if c.Name != "manager" {
				continue
			}
			for _, variable := range c.Env {
				if !strings.HasPrefix(variable.Name, relatedImagePrefix) {
					continue
				}
				suffix := strings.TrimPrefix(variable.Name, relatedImagePrefix)
				if suffix == "" {
					problems = append(problems, "CSV contains an empty RELATED_IMAGE_ variable name")
					continue
				}
				images = append(images, imageEntry{
					Name:  strings.ToLower(strings.ReplaceAll(suffix, "_", "-")),
					Image: variable.Value,
				})
			}
		}
	}

	if len(managerImages) != 1 {
		problems = append(problems, fmt.Sprintf("expected exactly one manager container, found %d", len(managerImages)))
	} else {
		images = append([]imageEntry{{Name: "hyperfleet-operator", Image: managerImages[0]}}, images...)
	}
	return images, problems
}

func compareImages(leftLabel string, left []imageEntry, rightLabel string, right []imageEntry) []string {
	problems := []string{}
	leftByName, leftProblems := imageMap(leftLabel, left)
	rightByName, rightProblems := imageMap(rightLabel, right)
	problems = append(problems, leftProblems...)
	problems = append(problems, rightProblems...)

	for name, image := range leftByName {
		if actual, ok := rightByName[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s is missing %s entry %q", rightLabel, leftLabel, name))
		} else if actual != image {
			problems = append(problems, fmt.Sprintf("%s entry %q is %q, expected %q", rightLabel, name, actual, image))
		}
	}
	for name := range rightByName {
		if _, ok := leftByName[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s has undeclared entry %q", rightLabel, name))
		}
	}
	return problems
}

func imageMap(label string, images []imageEntry) (map[string]string, []string) {
	result := make(map[string]string, len(images))
	seenImages := make(map[string]string, len(images))
	problems := []string{}
	for _, image := range images {
		if previous, ok := result[image.Name]; ok {
			problems = append(problems,
				fmt.Sprintf("%s has duplicate name %q (%q and %q)", label, image.Name, previous, image.Image),
			)
			continue
		}
		if previous, ok := seenImages[image.Image]; ok {
			problems = append(problems,
				fmt.Sprintf("%s has duplicate image %q (%s and %s)", label, image.Image, previous, image.Name),
			)
		}
		result[image.Name] = image.Image
		seenImages[image.Image] = image.Name
		if !isSHA256DigestPullspec(image.Image) {
			problems = append(problems, fmt.Sprintf("mutable or malformed %s image %q: %q", label, image.Name, image.Image))
		}
	}
	return result, problems
}

func isSHA256DigestPullspec(image string) bool {
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return false
	}
	digested, ok := named.(reference.Digested)
	if !ok {
		return false
	}
	digest := digested.Digest()
	return digest.Algorithm().String() == "sha256" && digest.Validate() == nil
}

func problemError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return errors.New("- " + strings.Join(problems, "\n- "))
}
