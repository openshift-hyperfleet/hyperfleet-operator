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

// verify-related-images checks the deployable image inventory against the
// manager image, RELATED_IMAGE_* values, and relatedImages in the bundle CSV.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

const relatedImagePrefix = "RELATED_IMAGE_"

var digestPullspec = regexp.MustCompile(`^\S+@sha256:[0-9a-f]{64}$`)

type imageEntry struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type inventoryDocument struct {
	Images []imageEntry `json:"images"`
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
	csvPath := flag.String("csv", "bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml", "path to the bundle CSV")
	inventoryPath := flag.String("inventory", "config/deployable-images.yaml", "path to the deployable image inventory")
	flag.Parse()

	csvData, err := readFile("CSV", *csvPath)
	if err == nil {
		err = verifyFiles(csvData, *inventoryPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "related image verification failed:\n%s\n", err)
		os.Exit(1)
	}
	fmt.Println("related image verification passed")
}

func verifyFiles(csvData []byte, inventoryPath string) error {
	inventoryData, err := readFile("deployable image inventory", inventoryPath)
	if err != nil {
		return err
	}
	var inventory inventoryDocument
	if err := yaml.Unmarshal(inventoryData, &inventory); err != nil {
		return fmt.Errorf("parse deployable image inventory YAML: %w", err)
	}
	return verifyCSV(csvData, inventory.Images)
}

func readFile(description, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", description, err)
	}
	return data, nil
}

func verifyCSV(data []byte, inventory []imageEntry) error {
	var csv csvDocument
	if err := yaml.Unmarshal(data, &csv); err != nil {
		return fmt.Errorf("parse CSV YAML: %w", err)
	}

	problems := validateInventory(inventory)
	csvImages, csvProblems := deployableImages(csv)
	problems = append(problems, csvProblems...)
	problems = append(problems, compareImages("deployable image inventory", inventory, "CSV image sources", csvImages)...)
	problems = append(problems, compareImages("CSV image sources", csvImages, "CSV spec.relatedImages", csv.Spec.RelatedImages)...)
	return problemError(problems)
}

func validateInventory(inventory []imageEntry) []string {
	if len(inventory) == 0 {
		return []string{"deployable image inventory is empty"}
	}
	problems := []string{}
	seenNames := map[string]bool{}
	seenImages := map[string]bool{}
	for _, entry := range inventory {
		if entry.Name == "" || entry.Image == "" {
			problems = append(problems, fmt.Sprintf("inventory entry %q has an empty name or image", entry.Name))
		}
		if seenNames[entry.Name] {
			problems = append(problems, fmt.Sprintf("duplicate deployable image name %q", entry.Name))
		}
		seenNames[entry.Name] = true
		if seenImages[entry.Image] {
			problems = append(problems, fmt.Sprintf("duplicate deployable image %q", entry.Image))
		}
		seenImages[entry.Image] = true
		if !digestPullspec.MatchString(entry.Image) {
			problems = append(problems, fmt.Sprintf("mutable or malformed inventory image %q: %q", entry.Name, entry.Image))
		}
	}
	return problems
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
			problems = append(problems, fmt.Sprintf("%s has duplicate name %q (%q and %q)", label, image.Name, previous, image.Image))
			continue
		}
		if previous, ok := seenImages[image.Image]; ok {
			problems = append(problems, fmt.Sprintf("%s has duplicate image %q (%s and %s)", label, image.Image, previous, image.Name))
		}
		result[image.Name] = image.Image
		seenImages[image.Image] = image.Name
		if !digestPullspec.MatchString(image.Image) {
			problems = append(problems, fmt.Sprintf("mutable or malformed %s image %q: %q", label, image.Name, image.Image))
		}
	}
	return result, problems
}

func problemError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return errors.New("- " + strings.Join(problems, "\n- "))
}
