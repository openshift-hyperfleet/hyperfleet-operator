// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
)

var bearerPattern = regexp.MustCompile(`(?i)Bearer\s+[^\s"']+`)
var jwtPattern = regexp.MustCompile(`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)

func redact(data string, secrets []string) string {
	var values []string
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		values = append(values, secret, base64.StdEncoding.EncodeToString([]byte(secret)),
			url.QueryEscape(secret), string(encoded[1:len(encoded)-1]))
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, value := range values {
		data = strings.ReplaceAll(data, value, "[REDACTED]")
	}
	data = bearerPattern.ReplaceAllString(data, "Bearer [REDACTED]")
	return jwtPattern.ReplaceAllString(data, "[REDACTED JWT]")
}

// diagnostics deliberately avoids serializing full Kubernetes objects: their
// annotations, environment, ConfigMaps and Secret data can contain credentials.
func (h *harness) diagnostics(ctx context.Context) error {
	if h.state.NamespaceUID == "" {
		return nil
	}
	secretList, err := h.clients.kube.CoreV1().Secrets(h.config.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("cannot read redaction inputs; logs were not collected")
	}
	var secrets []string
	for _, secret := range secretList.Items {
		for _, value := range secret.Data {
			secrets = append(secrets, string(value))
		}
	}
	if path := h.config.Scenario.DataProbe.TokenFile; path != "" {
		token, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("cannot read probe token for log redaction")
		}
		secrets = append(secrets, strings.TrimSpace(string(token)))
	}
	var errors []error
	var resources []map[string]any
	for _, gvr := range []schema.GroupVersionResource{catalogResource, subResource, planResource, csvResource} {
		list, readErr := h.resource(gvr).List(ctx, metav1.ListOptions{})
		if readErr != nil {
			errors = append(errors, fmt.Errorf("collect %s: %w", gvr.Resource, readErr))
			continue
		}
		for _, obj := range list.Items {
			resources = append(resources, map[string]any{fieldKind: obj.GetKind(), fieldName: obj.GetName(),
				fieldUID:          obj.GetUID(),
				"resourceVersion": obj.GetResourceVersion(), fieldPhase: stringField(&obj, "status", fieldPhase),
				"installedCSV": stringField(&obj, "status", "installedCSV"), "version": stringField(&obj,
					fieldSpec, "version")})
		}
	}
	errors = append(errors, h.writeJSON("olm.json", resources))
	pods, err := h.clients.kube.CoreV1().Pods(h.config.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return combineErrors(joinErrors(errors), err)
	}
	var summaries []map[string]any
	for _, pod := range pods.Items {
		var containers []map[string]any
		for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...),
			pod.Status.InitContainerStatuses...) {
			entry := map[string]any{fieldName: status.Name, fieldImage: status.Image, "imageID": status.ImageID,
				"ready": status.Ready, "restartCount": status.RestartCount}
			if status.State.Terminated != nil {
				entry["exitCode"] = status.State.Terminated.ExitCode
				entry["reason"] = status.State.Terminated.Reason
			}
			if status.State.Waiting != nil {
				entry["reason"] = status.State.Waiting.Reason
			}
			containers = append(containers, entry)
		}
		summaries = append(summaries, map[string]any{fieldName: pod.Name, fieldUID: pod.UID,
			"node": pod.Spec.NodeName, fieldPhase: pod.Status.Phase, "containers": containers})
		for _, container := range podImages(pod.Spec) {
			logs, readErr := h.clients.kube.CoreV1().Pods(h.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: container.Name, TailLines: ptr.To[int64](200), LimitBytes: ptr.To[int64](32768),
			}).DoRaw(ctx)
			// Pending containers legitimately have no logs; their structured status
			// is still retained. Never print an API error which may echo log data.
			if readErr != nil {
				continue
			}
			path := filepath.Join(h.config.ArtifactDir, pod.Name+"-"+container.Name+".log")
			errors = append(errors, os.WriteFile(path, []byte(redact(string(logs), secrets)), 0600))
		}
	}
	errors = append(errors, h.writeJSON("pods.json", summaries))
	version, err := h.clients.kube.Discovery().ServerVersion()
	if err == nil {
		errors = append(errors, h.writeJSON("cluster-version.json", version))
	}
	return joinErrors(errors)
}

func joinErrors(errors []error) error {
	var result error
	for _, err := range errors {
		result = combineErrors(result, err)
	}
	return result
}
