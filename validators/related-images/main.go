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
				Containers []container `json:"containers"`
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

	// Exits with status code 1 when passing in csvPath
	// operator-sdk reads errors from the JSON output; standalone -csv mode needs a non-zero exit code.
	// operator-sdk bundle validate ./bundle --alpha-select-external will fail on errors in json output
	if *csvPath != "" && len(result.Errors) > 0 {
		os.Exit(1)
	}
}

func validate(bundleRoot string) manifestResult {
	csvPath := findCSV(bundleRoot)
	if csvPath == "" {
		return manifestResult{
			Name:   validatorName,
			Errors: []validationMsg{errMsg("", "no ClusterServiceVersion found in bundle manifests")},
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

	containerImages := make(map[string]bool)
	for _, dep := range csv.Spec.Install.Spec.Deployments {
		for _, c := range dep.Spec.Template.Spec.Containers {
			// Add these to a managerImages map
			containerImages[c.Image] = false
			if !isSHA256DigestPullspec(c.Image) {
				result.Errors = append(result.Errors, errMsg("relatedImages",
					fmt.Sprintf("not using sha256 in image tag: %v", c.Image)))
			}
		}
	}

	relatedImages := make(map[string]string)
	for _, image := range csv.Spec.RelatedImages {
		if !isSHA256DigestPullspec(image.Image) {
			result.Errors = append(result.Errors, errMsg("relatedImages",
				fmt.Sprintf("not using sha256 in image tag: %v", image.Image)))
		}
		_, ok := relatedImages[image.Image]
		if !ok {
			// no match in the map, add them and continue
			relatedImages[image.Image] = image.Name

			// check if relatedImage is a container image, if so, mark it as added.
			if _, ok := containerImages[image.Image]; ok {
				containerImages[image.Image] = true
			}
		} else {
			// duplicate in map, add to result.Errors
			result.Errors = append(result.Errors, errMsg("relatedImages",
				fmt.Sprintf("duplicate value of image: %v", image.Image)))
		}
	}

	for _, dep := range csv.Spec.Install.Spec.Deployments {
		for _, c := range dep.Spec.Template.Spec.Containers {
			envRelatedImages := []string{}
			for _, e := range c.Env {
				if strings.HasPrefix(e.Name, "RELATED_IMAGE_") {
					if !isSHA256DigestPullspec(e.Value) {
						result.Errors = append(result.Errors, errMsg("relatedImages",
							fmt.Sprintf("not using sha256 in image tag: %v", e.Value)))
					}
					if slices.Contains(envRelatedImages, e.Name) {
						result.Errors = append(result.Errors,
							errMsg("relatedImages",
								fmt.Sprintf("env var duplicated: %v", e)))
						continue
					}
					envRelatedImages = append(envRelatedImages, e.Name)
					if _, ok := relatedImages[e.Value]; !ok {
						result.Errors = append(result.Errors,
							errMsg("relatedImages",
								fmt.Sprintf("env var not add to relatedImages: %v", e)))
					}
				}
			}
		}
	}

	// Verify that containerImages all include true
	for image, matched := range containerImages {
		if !matched {
			result.Errors = append(result.Errors, errMsg("spec.relatedImages",
				fmt.Sprintf("container image not in relatedImages:%q", image)))
		}
	}
	// check that images are all sha digests
	return result
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

func findCSV(bundleRoot string) string {
	manifestsDir := bundleRoot + "/manifests"
	entries, err := os.ReadDir(manifestsDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".clusterserviceversion.yaml") {
			return manifestsDir + "/" + e.Name()
		}
	}
	return ""
}

func errMsg(field, detail string) validationMsg {
	return validationMsg{
		Type:   "FailedValidation",
		Level:  "error",
		Field:  field,
		Detail: detail,
	}
}
