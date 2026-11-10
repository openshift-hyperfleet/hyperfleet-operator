// Copyright 2026. Licensed under the Apache License, Version 2.0.

package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	probeOwnerLabel = "upgrade_run"
	fieldLabels     = "labels"
)

type probeRequestFunc func(context.Context, ProbeConfig, string, string, map[string]any) (map[string]any, int, error)

func (h *harness) forward(ctx context.Context, p ProbeConfig) (string, func(), error) {
	service, err := h.clients.kube.CoreV1().Services(h.config.Namespace).Get(ctx, p.Service, metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	if len(service.Spec.Selector) == 0 {
		return "", nil, fmt.Errorf("probe Service must select pods")
	}
	pods, err := h.clients.kube.CoreV1().Pods(h.config.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: labels.SelectorFromSet(service.Spec.Selector).String()})
	if err != nil {
		return "", nil, err
	}
	podName := ""
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				podName = pod.Name
				break
			}
		}
		if podName != "" {
			break
		}
	}
	if podName == "" {
		return "", nil, fmt.Errorf("probe Service has no ready pod")
	}
	transport, upgrader, err := spdy.RoundTripperFor(h.clients.rest)
	if err != nil {
		return "", nil, err
	}
	endpoint, err := url.Parse(h.clients.rest.Host)
	if err != nil {
		return "", nil, err
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path,
		"/") + "/api/v1/namespaces/" + h.config.Namespace + "/pods/" + podName + "/portforward"
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport, Timeout: 30 * time.Second},
		http.MethodPost, endpoint)
	stop, ready := make(chan struct{}), make(chan struct{})
	var once sync.Once
	closeForward := func() { once.Do(func() { close(stop) }) }
	fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"},
		[]string{"0:" + strconv.Itoa(p.Port)}, stop, ready, io.Discard, io.Discard)
	if err != nil {
		return "", nil, err
	}
	finished := make(chan error, 1)
	go func() { finished <- fw.ForwardPorts() }()
	select {
	case <-ctx.Done():
		closeForward()
		return "", nil, ctx.Err()
	case err = <-finished:
		closeForward()
		return "", nil, fmt.Errorf("API port-forward stopped before readiness: %v", err)
	case <-ready:
	}
	ports, err := fw.GetPorts()
	if err != nil || len(ports) != 1 {
		closeForward()
		return "", nil, fmt.Errorf("API port-forward did not expose one port")
	}
	return "127.0.0.1:" + strconv.Itoa(int(ports[0].Local)), closeForward, nil
}

func probeHTTPClient(p ProbeConfig) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if p.CAFile != "" {
		pem, err := os.ReadFile(p.CAFile)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("probe CA file contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots,
			ServerName: p.ServerName}
	}
	// Do not send localhost requests through environment-configured proxies.
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("probe redirects are not allowed")
		}}, nil
}

func (h *harness) probeRequest(ctx context.Context, p ProbeConfig, method, path string,
	body map[string]any) (map[string]any, int, error) {
	if h.probeCall != nil {
		return h.probeCall(ctx, p, method, path, body)
	}
	address, stop, err := h.forward(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	defer stop()
	client, err := probeHTTPClient(p)
	if err != nil {
		return nil, 0, err
	}
	defer client.CloseIdleConnections()
	scheme := "http"
	if p.CAFile != "" {
		scheme = "https"
	}
	var data []byte
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+address+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.TokenFile != "" {
		token, readErr := os.ReadFile(p.TokenFile)
		if readErr != nil {
			return nil, 0, readErr
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("API data probe transport failed")
	}
	defer response.Body.Close() //nolint:errcheck // Read errors below carry the request failure.
	data, err = io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read data probe response failed")
	}
	var result map[string]any
	if len(data) > 0 && json.Unmarshal(data, &result) != nil && method != http.MethodDelete {
		return nil, response.StatusCode, fmt.Errorf("data probe response is not JSON")
	}
	return result, response.StatusCode, nil
}

func recordFingerprint(record map[string]any) (string, error) {
	if record["id"] == nil || record[fieldName] == nil || record[fieldSpec] == nil {
		return "", fmt.Errorf("data probe record lacks id/name/spec")
	}
	data, err := json.Marshal(map[string]any{"id": record["id"], fieldName: record[fieldName],
		fieldSpec: record[fieldSpec], fieldLabels: record[fieldLabels]})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func (h *harness) createProbe(ctx context.Context) (string, error) {
	p := h.config.Scenario.DataProbe
	body := maps.Clone(p.Body)
	body[fieldName] = h.state.RunID
	probeLabels := map[string]any{}
	if value, exists := body[fieldLabels]; exists {
		original, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("data probe labels must be an object")
		}
		maps.Copy(probeLabels, original)
	}
	probeLabels[probeOwnerLabel] = h.state.RunID
	body[fieldLabels] = probeLabels
	// Record intent before POST: a failed response does not prove no row exists.
	h.state.Probe = p
	h.state.Probe.Body = nil
	h.state.ProbePending = true
	if err := h.saveState(); err != nil {
		return "", err
	}
	record, status, err := h.probeRequest(ctx, p, http.MethodPost, p.Collection, body)
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", fmt.Errorf("create data probe returned HTTP %d", status)
	}
	id, err := probeIdentity(record, h.state.RunID)
	if err != nil {
		return "", err
	}
	h.state.ProbeID = id
	h.state.ProbePending = false
	if err = h.saveState(); err != nil {
		return "", err
	}
	return recordFingerprint(record)
}

func (h *harness) verifyProbe(ctx context.Context, expected string) error {
	p := h.state.Probe
	record, status, err := h.probeRequest(ctx, p, http.MethodGet, strings.TrimSuffix(p.Collection,
		"/")+"/"+url.PathEscape(h.state.ProbeID), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("read preserved data returned HTTP %d", status)
	}
	fingerprint, err := recordFingerprint(record)
	if err != nil {
		return err
	}
	if fingerprint != expected {
		return fmt.Errorf("pre-upgrade data was not preserved")
	}
	return h.writeJSON("data-survival.json", map[string]string{"id": h.state.ProbeID, "sha256": fingerprint})
}

func (h *harness) deleteProbe(ctx context.Context) error {
	if h.state.ProbeID == "" && !h.state.ProbePending {
		return nil
	}
	// Leave time for Kubernetes teardown if record recovery/finalization stalls.
	budget := min(h.config.Timeout, 2*time.Minute)
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if h.state.ProbeID == "" {
		if err := h.recoverProbe(ctx); err != nil {
			return err
		}
	}
	p := h.state.Probe
	path := strings.TrimSuffix(p.Collection, "/") + "/" + url.PathEscape(h.state.ProbeID)
	_, status, err := h.probeRequest(ctx, p, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusAccepted &&
		status != http.StatusOK && status != http.StatusNotFound {
		return fmt.Errorf("delete data probe returned HTTP %d", status)
	}
	if status != http.StatusNotFound {
		if err = h.poll(ctx, "data probe finalization", func(ctx context.Context) (bool, error) {
			_, code, readErr := h.probeRequest(ctx, p, http.MethodGet, path, nil)
			if readErr != nil {
				return false, readErr
			}
			if code != http.StatusOK && code != http.StatusNotFound {
				return false, fmt.Errorf("verify data probe deletion returned HTTP %d", code)
			}
			return code == http.StatusNotFound, nil
		}); err != nil {
			return err
		}
	}
	h.state.ProbeID = ""
	h.state.ProbePending = false
	h.state.Probe = ProbeConfig{}
	return h.saveState()
}

func probeIdentity(record map[string]any, runID string) (string, error) {
	id, ok := record["id"].(string)
	probeLabels, labelsOK := record[fieldLabels].(map[string]any)
	if !ok || id == "" || record[fieldName] != runID || !labelsOK || probeLabels[probeOwnerLabel] != runID {
		return "", fmt.Errorf("data probe identity or ownership is invalid")
	}
	return id, nil
}

func (h *harness) recoverProbe(ctx context.Context) error {
	// The name and label are internally generated DNS labels, not arbitrary
	// Tree Search Language (TSL) expressions accepted by the API search endpoint.
	if len(validation.IsDNS1123Label(h.state.RunID)) != 0 {
		return fmt.Errorf("invalid run identity in probe receipt")
	}
	p := h.state.Probe
	query := url.Values{"search": {"name='" + h.state.RunID + "' and labels." + probeOwnerLabel +
		"='" + h.state.RunID + "'"}, "size": {"2"}}
	return h.poll(ctx, "recover pending data probe", func(ctx context.Context) (bool, error) {
		result, code, err := h.probeRequest(ctx, p, http.MethodGet, p.Collection+"?"+query.Encode(), nil)
		if err != nil {
			return false, err
		}
		if code != http.StatusOK {
			return false, fmt.Errorf("recover data probe returned HTTP %d", code)
		}
		items, ok := result["items"].([]any)
		if !ok {
			return false, fmt.Errorf("data probe lookup has no items array")
		}
		if len(items) == 0 {
			// POST may still commit. An empty lookup cannot safely clear intent.
			return false, nil
		}
		if len(items) != 1 {
			return false, fmt.Errorf("data probe lookup is ambiguous")
		}
		record, ok := items[0].(map[string]any)
		if !ok {
			return false, fmt.Errorf("invalid data probe lookup item")
		}
		id, err := probeIdentity(record, h.state.RunID)
		if err != nil {
			return false, err
		}
		h.state.ProbeID = id
		h.state.ProbePending = false
		return true, h.saveState()
	})
}
