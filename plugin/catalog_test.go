package main

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
)

var imageModelIDs = []string{"Hy-Image-3.5-preview", "doubao-seedream-5-lite", "qwen-image-3.0-pro"}

func TestImageCatalogAndChatRegistration(t *testing.T) {
	var posts atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			posts.Add(1)
			t.Error("unexpected mutation")
			return
		}
		rows := []map[string]any{{"id": "spark-x2.6", "type": "chat"}, {"id": "auto"}, {"id": "unknown-image", "type": "image"}}
		for _, id := range imageModelIDs {
			rows = append(rows, map[string]any{"id": id, "type": "image", "reasoning_efforts": []string{"high"}})
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	auth, _ := json.Marshal(map[string]string{"session": "mock-catalog-only"})
	req, _ := json.Marshal(map[string]any{"StorageJSON": auth})
	var env envelope
	_ = json.Unmarshal(handleModelsForAuth(req), &env)
	var result struct{ Models []map[string]any }
	_ = json.Unmarshal(env.Result, &result)
	if len(result.Models) != 4 {
		t.Fatalf("wrong catalogue: %s", env.Result)
	}
	found := map[string]bool{}
	for _, m := range result.Models {
		id := m["ID"].(string)
		found[id] = true
		if id == "lm/spark-x2.6" {
			if m["Type"] != "chat" || len(m["SupportedGenerationMethods"].([]any)) != 1 {
				t.Fatal("chat capability changed")
			}
		} else {
			if m["Type"] != "openai-image" || len(m["SupportedGenerationMethods"].([]any)) != 1 || m["Thinking"] != nil {
				t.Fatalf("image registration incorrect: %+v", m)
			}
		}
	}
	for _, id := range imageModelIDs {
		if !found[modelPrefix+id] {
			t.Fatal("missing image", id)
		}
	}
	models, err := fetchGatewayModels("mock-catalog-only", false)
	if err != nil {
		t.Fatal(err)
	}
	rows := toModelRows(models)
	if len(rows) != 4 {
		t.Fatal("management catalogue omitted image")
	}
	for _, row := range rows {
		if isLoomyImageModel(row.ID) && (row.Tools || len(row.Efforts) != 0) {
			t.Fatal("incorrect image capabilities")
		}
	}
	if posts.Load() != 0 {
		t.Fatal("unexpected POST")
	}
}

func TestImagesRejectChatExecutionAndSelfTestsLocally(t *testing.T) {
	var hits atomic.Int32
	withCatalogServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		t.Error("catalogue-only execution reached upstream")
	})
	for _, id := range imageModelIDs {
		for _, model := range []string{id, modelPrefix + id} {
			for _, stream := range []bool{false, true} {
				req, _ := json.Marshal(execRequest{Model: model})
				var env envelope
				_ = json.Unmarshal(handleExecute(req, stream), &env)
				if env.Error == nil || env.Error.Code != "unsupported_image_request" || env.Error.HTTPStatus != 400 {
					t.Fatalf("not rejected: %+v", env)
				}
			}
			req, _ := json.Marshal(map[string]string{"model": model})
			var env envelope
			_ = json.Unmarshal(handleSelfTest(req), &env)
			var result struct{ StatusCode int }
			_ = json.Unmarshal(env.Result, &result)
			if result.StatusCode != 400 {
				t.Fatalf("selftest not locally rejected: %s", env.Result)
			}
		}
		res := testAccount(hostAuthEntry{}, id)
		if res.Status != 400 {
			t.Fatal("direct account test not locally rejected")
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("unexpected upstream traffic: %d", hits.Load())
	}
}

func TestImageTypeMissingDoesNotBecomeChat(t *testing.T) {
	for _, id := range imageModelIDs {
		raw, _ := json.Marshal([]map[string]string{{"id": id}})
		models, _, err := parseGatewayModels(raw)
		if err != nil || len(models) != 0 {
			t.Fatalf("bad directory classification: %+v %v", models, err)
		}
	}
}
