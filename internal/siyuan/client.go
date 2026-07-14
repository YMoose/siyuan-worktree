package siyuan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Notebook struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Sort   int    `json:"sort"`
	Closed bool   `json:"closed"`
}

type Document struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	SubFileCount int    `json:"subFileCount"`
	Sort         int    `json:"sort"`
}

type ChildBlock struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	SubType string `json:"subType"`
}

type TransactionOperation struct {
	Action     string   `json:"action"`
	ID         string   `json:"id"`
	RootID     string   `json:"rootID"`
	ParentID   string   `json:"parentID"`
	PreviousID string   `json:"previousID"`
	NextID     string   `json:"nextID"`
	BlockIDs   []string `json:"blockIDs"`
	BlockID    string   `json:"blockID"`
}

type Transaction struct {
	Timestamp      int64                  `json:"timestamp"`
	DoOperations   []TransactionOperation `json:"doOperations"`
	UndoOperations []TransactionOperation `json:"undoOperations"`
}

type MutationReceipt struct {
	ReceivedAt   time.Time     `json:"receivedAt"`
	Transactions []Transaction `json:"transactions"`
}

func (r MutationReceipt) BlockIDs() []string {
	seen := map[string]bool{}
	var result []string
	for _, transaction := range r.Transactions {
		for _, operation := range transaction.DoOperations {
			candidates := make([]string, 0, 2+len(operation.BlockIDs))
			candidates = append(candidates, operation.ID, operation.BlockID)
			candidates = append(candidates, operation.BlockIDs...)
			for _, id := range candidates {
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				result = append(result, id)
			}
		}
	}
	return result
}

type HistorySearchResult struct {
	Histories  []string `json:"histories"`
	PageCount  int      `json:"pageCount"`
	TotalCount int      `json:"totalCount"`
}

type API interface {
	ListNotebooks(context.Context) ([]Notebook, error)
	ListDocuments(context.Context, string, string) ([]Document, error)
	GetChildBlocks(context.Context, string) ([]ChildBlock, error)
	GetBlockKramdown(context.Context, string) (string, error)
	GetBlockKramdowns(context.Context, []string) (map[string]string, error)
	BatchGetBlockAttrs(context.Context, []string) (map[string]map[string]string, error)
	CreateDocHistory(context.Context, string) error
	SearchHistory(context.Context, string) (HistorySearchResult, error)
	InsertBlock(context.Context, string, string, string, string) (MutationReceipt, error)
	DeleteBlock(context.Context, string) (MutationReceipt, error)
	UpdateBlock(context.Context, string, string) (MutationReceipt, error)
}

func (c *Client) GetChildBlocks(ctx context.Context, id string) ([]ChildBlock, error) {
	var data []ChildBlock
	if err := c.post(ctx, "/api/block/getChildBlocks", map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

type APIError struct {
	Path       string
	HTTPStatus int
	Message    string
}

func (e *APIError) Error() string {
	if e.HTTPStatus != 0 {
		return fmt.Sprintf("SiYuan API %s returned HTTP %d", e.Path, e.HTTPStatus)
	}
	return fmt.Sprintf("SiYuan API %s failed: %s", e.Path, e.Message)
}

type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) ListNotebooks(ctx context.Context) ([]Notebook, error) {
	var data struct {
		Notebooks []Notebook `json:"notebooks"`
	}
	if err := c.post(ctx, "/api/notebook/lsNotebooks", map[string]any{}, &data); err != nil {
		return nil, err
	}
	return data.Notebooks, nil
}

func (c *Client) ListDocuments(ctx context.Context, notebookID, path string) ([]Document, error) {
	var data struct {
		Files []Document `json:"files"`
	}
	body := map[string]any{
		"notebook":          notebookID,
		"path":              path,
		"maxListCount":      0,
		"ignoreMaxListHint": true,
	}
	if err := c.post(ctx, "/api/filetree/listDocsByPath", body, &data); err != nil {
		return nil, err
	}
	return data.Files, nil
}

func (c *Client) GetBlockKramdown(ctx context.Context, id string) (string, error) {
	var data struct {
		Kramdown string `json:"kramdown"`
	}
	if err := c.post(ctx, "/api/block/getBlockKramdown", map[string]any{"id": id, "mode": "md"}, &data); err != nil {
		return "", err
	}
	return data.Kramdown, nil
}

func (c *Client) GetBlockKramdowns(ctx context.Context, ids []string) (map[string]string, error) {
	data := map[string]string{}
	if len(ids) == 0 {
		return data, nil
	}
	if err := c.post(ctx, "/api/block/getBlockKramdowns", map[string]any{"ids": ids, "mode": "md"}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) BatchGetBlockAttrs(ctx context.Context, ids []string) (map[string]map[string]string, error) {
	data := map[string]map[string]string{}
	if len(ids) == 0 {
		return data, nil
	}
	if err := c.post(ctx, "/api/attr/batchGetBlockAttrs", map[string]any{"ids": ids}, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) FlushTransactions(ctx context.Context) error {
	return c.post(ctx, "/api/sqlite/flushTransaction", map[string]any{}, nil)
}

func (c *Client) CreateDocHistory(ctx context.Context, documentID string) error {
	return c.post(ctx, "/api/history/createDocHistory", map[string]any{"id": documentID}, nil)
}

func (c *Client) SearchHistory(ctx context.Context, documentID string) (HistorySearchResult, error) {
	var result HistorySearchResult
	err := c.post(ctx, "/api/history/searchHistory", map[string]any{
		"query": documentID,
		"type":  3,
		"op":    "update",
		"page":  1,
	}, &result)
	return result, err
}

func (c *Client) InsertBlock(ctx context.Context, markdown, parentID, previousID, nextID string) (MutationReceipt, error) {
	return c.mutate(ctx, "/api/block/insertBlock", map[string]any{
		"dataType":   "markdown",
		"data":       markdown,
		"parentID":   parentID,
		"previousID": previousID,
		"nextID":     nextID,
	})
}

func (c *Client) DeleteBlock(ctx context.Context, id string) (MutationReceipt, error) {
	return c.mutate(ctx, "/api/block/deleteBlock", map[string]any{"id": id})
}

func (c *Client) UpdateBlock(ctx context.Context, id, markdown string) (MutationReceipt, error) {
	return c.mutate(ctx, "/api/block/updateBlock", map[string]any{
		"id":       id,
		"dataType": "markdown",
		"data":     markdown,
	})
}

func (c *Client) mutate(ctx context.Context, path string, body any) (MutationReceipt, error) {
	receipt := MutationReceipt{ReceivedAt: time.Now().UTC()}
	if err := c.post(ctx, path, body, &receipt.Transactions); err != nil {
		return MutationReceipt{}, err
	}
	receipt.ReceivedAt = time.Now().UTC()
	return receipt, nil
}

func (c *Client) post(ctx context.Context, path string, body, destination any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Token "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("call SiYuan API %s: %w", path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		io.Copy(io.Discard, response.Body)
		return &APIError{Path: path, HTTPStatus: response.StatusCode}
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode SiYuan API %s: %w", path, err)
	}
	if envelope.Code != 0 {
		message := envelope.Msg
		if message == "" {
			message = fmt.Sprintf("code %d", envelope.Code)
		}
		return &APIError{Path: path, Message: message}
	}
	if destination == nil || len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, destination); err != nil {
		return fmt.Errorf("decode SiYuan API %s data: %w", path, err)
	}
	return nil
}
