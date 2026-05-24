package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func loadConfig() config {
	baseURL := strings.TrimSpace(os.Getenv("ATM_API_URL"))
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8080/api/v1"
	}
	apiKey := strings.TrimSpace(os.Getenv("ATM_API_KEY"))
	raw := strings.EqualFold(strings.TrimSpace(os.Getenv("ATM_OUTPUT")), "raw")
	return config{BaseURL: baseURL, APIKey: apiKey, Raw: raw}
}

func (c client) get(path string, out any) error {
	return c.doJSON(http.MethodGet, path, nil, out)
}

func (c client) postJSON(path string, body any, out any) error {
	return c.doJSON(http.MethodPost, path, body, out)
}

func (c client) patchJSON(path string, body any, out any) error {
	return c.doJSON(http.MethodPatch, path, body, out)
}

func (c client) doJSON(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c client) uploadFile(path, filePath string) (uploadResponse, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return uploadResponse{}, err
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return uploadResponse{}, err
	}
	if _, err := io.Copy(part, file); err != nil {
		return uploadResponse{}, err
	}
	if err := writer.Close(); err != nil {
		return uploadResponse{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+path, &body)
	if err != nil {
		return uploadResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return uploadResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return uploadResponse{}, decodeAPIError(resp)
	}
	var out uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return uploadResponse{}, err
	}
	return out, nil
}

func (c client) downloadFile(path, outputPath string) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeAPIError(resp)
	}
	out, err := os.Create(outputPath)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func decodeAPIError(resp *http.Response) error {
	var payload apiError
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.Error != "" {
		if payload.Code != "" {
			return fmt.Errorf("%s: %s", payload.Code, payload.Error)
		}
		return errors.New(payload.Error)
	}
	return fmt.Errorf("request failed: %s", resp.Status)
}
