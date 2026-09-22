// Copyright 2026.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	_ "crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"sigs.k8s.io/yaml"
)

const (
	relatedImagePrefix = "RELATED_IMAGE_"
	validatorName      = "related-images"
)

// operator-sdk external validator JSON output types
type manifestResult struct {
	Name     string          `json:"name"`
	Errors   []validationMsg `json:"errors"`
	Warnings []validationMsg `json:"warnings"`
}

type validationMsg struct {
	Type   string `json:"type"`
	Level  string `json:"level"`
	Field  string `json:"field,omitempty"`
	Detail string `json:"detail"`
}

type imageEntry struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type csvDocument struct {
	Spec struct {
		Install struct {
			Spec struct {
				Deployments []deployment `json:"deployments"`
			} `json:"spec"`
		} `json:"install"`
		RelatedImages []imageEntry `json:"relatedImages"`
	} `json:"spec"`
}

type deployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers     []container `json:"containers"`
				InitContainers []container `json:"initContainers"`
			} `json:"spec"`
		} `json:"template"`
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
	csvPath := flag.String("csv", "", "path to a CSV file (standalone mode)")
	flag.Parse()

	var result manifestResult
	if *csvPath != "" {
		data, err := os.ReadFile(*csvPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read CSV: %v\n", err)
			os.Exit(1)
		}
		result = validateCSVData(data)
	} else if flag.NArg() > 0 {
		result = validate(flag.Arg(0))
	} else {
		fmt.Fprintf(os.Stderr, "usage: %s <bundle root>\n       %s -csv <csv file>\n", os.Args[0], os.Args[0])
		os.Exit(1)
	}

	out, err := json.MarshalIndent(result, "", "    ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR marshaling result: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))

	// operator-sdk reads errors from JSON output, so exit 0 is fine there.
	// Standalone -csv mode needs a non-zero exit code for CI.
	if *csvPath != "" && len(result.Errors) > 0 {
		os.Exit(1)
	}
}

func validate(bundleRoot string) manifestResult {
	csvPath, err := findCSV(bundleRoot)
	if err != nil {
		return manifestResult{
			Name:   validatorName,
			Errors: []validationMsg{errMsg("", fmt.Sprintf("failed to find CSV: %v", err))},
		}
	}

	data, err := os.ReadFile(csvPath)
	if err != nil {
		return manifestResult{
			Name:   validatorName,
			Errors: []validationMsg{errMsg("", fmt.Sprintf("failed to read CSV: %v", err))},
		}
	}

	return validateCSVData(data)
}

func validateCSVData(data []byte) manifestResult {
	result := manifestResult{Name: validatorName}

	var csv csvDocument
	if err := yaml.Unmarshal(data, &csv); err != nil {
		result.Errors = append(result.Errors, errMsg("", fmt.Sprintf("failed to parse CSV: %v", err)))
		return result
	}

	containerImages := validateContainerImages(&result, csv.Spec.Install.Spec.Deployments)
	relatedImages := validateRelatedImages(&result, csv.Spec.RelatedImages, containerImages)
	validateEnvVars(&result, csv.Spec.Install.Spec.Deployments, relatedImages)

	for image, matched := range containerImages {
		if !matched {
			result.Errors = append(result.Errors, errMsg("spec.relatedImages",
				fmt.Sprintf("container image not in relatedImages: %q", image)))
		}
	}
	return result
}

// validateContainerImages checks that each container image is a sha256 digest pullspec.
func validateContainerImages(result *manifestResult, deployments []deployment) map[string]bool {
	containerImages := make(map[string]bool)
	for _, dep := range deployments {
		allContainers := append(dep.Spec.Template.Spec.Containers, dep.Spec.Template.Spec.InitContainers...)
		for _, c := range allContainers {
			containerImages[c.Image] = false
			if !isSHA256DigestPullspec(c.Image) {
				result.Errors = append(result.Errors, errMsg("relatedImages",
					fmt.Sprintf("image reference is not a sha256 digest: %v", c.Image)))
			}
		}
	}
	return containerImages
}

// validateRelatedImages checks for duplicate entries and non-sha256 digests in spec.relatedImages.
func validateRelatedImages(
	result *manifestResult, images []imageEntry, containerImages map[string]bool,
) map[string]string {
	relatedImages := make(map[string]string)
	for _, image := range images {
		if !isSHA256DigestPullspec(image.Image) {
			result.Errors = append(result.Errors, errMsg("relatedImages",
				fmt.Sprintf("image reference is not a sha256 digest: %v", image.Image)))
		}
		if _, ok := relatedImages[image.Image]; !ok {
			relatedImages[image.Image] = image.Name
			if _, ok := containerImages[image.Image]; ok {
				containerImages[image.Image] = true
			}
		} else {
			result.Errors = append(result.Errors, errMsg("relatedImages",
				fmt.Sprintf("duplicate value of image: %v", image.Image)))
		}
	}
	return relatedImages
}

// validateEnvVars checks that RELATED_IMAGE_ env vars use sha256 digests, are not duplicated
// within a container, and have a corresponding entry in spec.relatedImages.
func validateEnvVars(result *manifestResult, deployments []deployment, relatedImages map[string]string) {
	for _, dep := range deployments {
		allContainers := append(dep.Spec.Template.Spec.Containers, dep.Spec.Template.Spec.InitContainers...)
		for _, c := range allContainers {
			envRelatedImages := []string{}
			for _, e := range c.Env {
				if strings.HasPrefix(e.Name, relatedImagePrefix) {
					if !isSHA256DigestPullspec(e.Value) {
						result.Errors = append(result.Errors, errMsg("relatedImages",
							fmt.Sprintf("image reference is not a sha256 digest: %v", e.Value)))
					}
					if slices.Contains(envRelatedImages, e.Name) {
						result.Errors = append(result.Errors,
							errMsg("relatedImages",
								fmt.Sprintf("duplicate env var %s in container %s", e.Name, c.Name)))
						continue
					}
					envRelatedImages = append(envRelatedImages, e.Name)
					if _, ok := relatedImages[e.Value]; !ok {
						result.Errors = append(result.Errors,
							errMsg("relatedImages",
								fmt.Sprintf("env var %s not found in spec.relatedImages (value: %s)", e.Name, e.Value)))
					}
				}
			}
		}
	}
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

func findCSV(bundleRoot string) (string, error) {
	manifestsDir := bundleRoot + "/manifests"
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return "", fmt.Errorf("reading manifests directory: %w", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".clusterserviceversion.yaml") {
			return manifestsDir + "/" + e.Name(), nil
		}
	}
	return "", fmt.Errorf("no CSV found in manifests directory")
}

func errMsg(field, detail string) validationMsg {
	return validationMsg{
		Type:   "FailedValidation",
		Level:  "error",
		Field:  field,
		Detail: detail,
	}
}
