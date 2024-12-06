package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

type oaiClient struct {
	c      *http.Client
	apiKey string
}

type oaiRequest struct {
	Model          string             `json:"model"`
	Messages       []oaiMessage       `json:"messages"`
	ResponseFormat *oaiResponseFormat `json:"response_format"`
}

type oaiResponseFormat struct {
	Type       string                 `json:"type"`
	JSONSchema map[string]interface{} `json:"json_schema"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiResponse struct {
	Choices []oaiChoice `json:"choices"`
}

type oaiChoice struct {
	Message oaiMessage `json:"message"`
}

func newOAIClient(apiKey string) *oaiClient {
	return &oaiClient{
		c:      &http.Client{},
		apiKey: apiKey,
	}
}

func newOAIClientFromEnv() (*oaiClient, error) {
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is not set")
	}

	return newOAIClient(apiKey), nil
}

func (oai *oaiClient) completeChat(system string, in any, responseFormat *oaiResponseFormat, out any) error {
	user, err := json.Marshal(in)
	if err != nil {
		return err
	}

	reqBody, err := json.Marshal(&oaiRequest{
		Model: "gpt-4o",
		Messages: []oaiMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: string(user)},
		},
		ResponseFormat: responseFormat,
	})

	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", oai.apiKey))
	req.Header.Set("Content-Type", "application/json")

	resp, err := oai.c.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", string(body))
	}

	dec := json.NewDecoder(resp.Body)
	res := &oaiResponse{}

	err = dec.Decode(res)
	if err != nil {
		return err
	}

	err = json.Unmarshal([]byte(res.Choices[0].Message.Content), out)
	if err != nil {
		return err
	}

	return nil
}
