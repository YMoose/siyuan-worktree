package siyuan

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestClientSendsTokenAndDecodesEnvelope(t *testing.T) {
	client := newTestClient(t, "secret", func(request *http.Request) string {
		if got := request.Header.Get("Authorization"); got != "Token secret" {
			t.Fatalf("authorization = %q", got)
		}
		if request.URL.Path != "/api/notebook/lsNotebooks" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return `{"code":0,"msg":"","data":{"notebooks":[{"id":"n1","name":"Work"}]}}`
	})
	notebooks, err := client.ListNotebooks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notebooks) != 1 || notebooks[0].Name != "Work" {
		t.Fatalf("notebooks = %+v", notebooks)
	}
}

func TestCreateDocHistoryUsesDocumentRootID(t *testing.T) {
	client := newTestClient(t, "", func(request *http.Request) string {
		if request.URL.Path != "/api/history/createDocHistory" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(`"id":"20260714110000-doc0001"`)) {
			t.Fatalf("body = %s", body)
		}
		return `{"code":0,"msg":"","data":null}`
	})
	if err := client.CreateDocHistory(context.Background(), "20260714110000-doc0001"); err != nil {
		t.Fatal(err)
	}
}

func TestFlushTransactionsUsesKernelBarrier(t *testing.T) {
	client := newTestClient(t, "", func(request *http.Request) string {
		if request.URL.Path != "/api/sqlite/flushTransaction" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return `{"code":0,"msg":"","data":null}`
	})
	if err := client.FlushTransactions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSearchHistoryUsesDocumentIDFilter(t *testing.T) {
	client := newTestClient(t, "", func(request *http.Request) string {
		if request.URL.Path != "/api/history/searchHistory" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		for _, expected := range [][]byte{
			[]byte(`"query":"20260714110000-doc0001"`),
			[]byte(`"type":3`),
			[]byte(`"op":"update"`),
			[]byte(`"page":1`),
		} {
			if !bytes.Contains(body, expected) {
				t.Fatalf("body = %s, missing %s", body, expected)
			}
		}
		return `{"code":0,"msg":"","data":{"histories":["1784073600"],"pageCount":1,"totalCount":1}}`
	})
	result, err := client.SearchHistory(context.Background(), "20260714110000-doc0001")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Histories) != 1 || result.Histories[0] != "1784073600" || result.TotalCount != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestMutationReceiptDecodesKernelTransactions(t *testing.T) {
	client := newTestClient(t, "", func(request *http.Request) string {
		if request.URL.Path != "/api/block/insertBlock" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		return `{
			"code":0,
			"msg":"",
			"data":[{
				"timestamp":1784073600123,
				"doOperations":[{
					"action":"insert",
					"id":"20260714120000-new0001",
					"parentID":"20260714110000-doc0001",
					"previousID":"20260714115900-old0001"
				}],
				"undoOperations":[]
			}]
		}`
	})
	receipt, err := client.InsertBlock(context.Background(), "new", "20260714110000-doc0001", "20260714115900-old0001", "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ReceivedAt.IsZero() || len(receipt.Transactions) != 1 {
		t.Fatalf("receipt = %+v", receipt)
	}
	operation := receipt.Transactions[0].DoOperations[0]
	if operation.Action != "insert" || operation.ID != "20260714120000-new0001" || operation.ParentID != "20260714110000-doc0001" {
		t.Fatalf("operation = %+v", operation)
	}
	blockIDs := receipt.BlockIDs()
	if len(blockIDs) != 1 || blockIDs[0] != operation.ID {
		t.Fatalf("block IDs = %v", blockIDs)
	}
}

func newTestClient(t *testing.T, token string, response func(*http.Request) string) *Client {
	t.Helper()
	return &Client{
		endpoint: "http://siyuan.test",
		token:    token,
		http: &http.Client{
			Timeout: time.Second,
			Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(bytes.NewBufferString(response(request))),
					Request:    request,
				}, nil
			}),
		},
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
