package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestRequestMetadataDoesNotPersistRequestContent(t *testing.T) {
	dir := requestMetaTestDir(t)
	body := []byte(`{
		"model":"gpt-test",
		"input":"SECRET_USER_PROMPT",
		"tools":[
			{"type":"function","name":"web_search","description":"SECRET_TOOL_TEXT"},
			{"type":"x_search"}
		]
	}`)
	tools, webSearch, hostedWebSearch, functionWebSearch, xSearch := summarizeBody(body)
	if tools != 2 || webSearch != 1 || hostedWebSearch != 0 || functionWebSearch != 1 || xSearch != 1 {
		t.Fatalf("summary tools=%d web=%d hosted=%d function=%d x=%d", tools, webSearch, hostedWebSearch, functionWebSearch, xSearch)
	}

	request := facadeRequest{BuildXSearch: 1, ProxyAddedWebSearch: true, ClientSearchPrepared: true, ClientSearchAlias: clientWebSearchWireAliasBase}
	saveLastRequestMeta("https://example.test/v1/responses", "gpt-test", len(body), tools, webSearch, hostedWebSearch, functionWebSearch, xSearch, request)
	if _, err := os.Stat(filepath.Join(dir, "last_request.json")); !os.IsNotExist(err) {
		t.Fatalf("full request snapshot must not exist: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "last_request_meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("SECRET")) {
		t.Fatalf("metadata leaked request content: %s", data)
	}
	var meta requestMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Model != "gpt-test" || meta.Tools != 2 || meta.WebSearch != 1 ||
		meta.HostedWebSearch != 0 || meta.FunctionWebSearch != 1 || meta.XSearch != 1 ||
		meta.BuildXSearch != 1 || !meta.ProxyAddedWebSearch || !meta.ClientSearchPrepared || !meta.ClientSearchAliased {
		t.Fatalf("unexpected metadata: %+v", meta)
	}
}

func TestRequestMetadataWritesAreAtomicAndRedacted(t *testing.T) {
	dir := requestMetaTestDir(t)
	path := filepath.Join(dir, "last_request_meta.json")
	saveLastRequestMeta("https://api.example/tenant/SECRET_PATH/responses?api_key=SECRET_QUERY", "model", 1, 0, 0, 0, 0, 0, facadeRequest{})

	stop := make(chan struct{})
	readerDone := make(chan struct{})
	readErrors := make(chan string, 1)
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			if !json.Valid(data) {
				select {
				case readErrors <- "observed partial JSON":
				default:
				}
				return
			}
		}
	}()

	var writers sync.WaitGroup
	for index := 0; index < 64; index++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			saveLastRequestMeta("https://api.example/tenant/SECRET_PATH/responses?api_key=SECRET_QUERY", "model", 1, 0, 0, 0, 0, 0, facadeRequest{})
		}()
	}
	writers.Wait()
	close(stop)
	<-readerDone
	select {
	case message := <-readErrors:
		t.Fatal(message)
	default:
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("SECRET")) || !bytes.Contains(data, []byte(`"target":"https://api.example/.../responses"`)) {
		t.Fatalf("metadata target was not redacted: %s", data)
	}
}

func requestMetaTestDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", root)
		return filepath.Join(root, "hellogrok")
	}
	t.Setenv("HOME", root)
	return filepath.Join(root, ".hellogrok")
}

func TestDecodeRequestObjectRejectsNull(t *testing.T) {
	if _, err := decodeRequestObject([]byte("null")); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("null request error = %v", err)
	}
}

func TestSummarizeBodyCountsHostedWebSearch(t *testing.T) {
	body := []byte(`{"tools":[{"type":"web_search","filters":{"allowed_domains":null}}]}`)
	tools, webSearch, hostedWebSearch, functionWebSearch, xSearch := summarizeBody(body)
	if tools != 1 || webSearch != 1 || hostedWebSearch != 1 || functionWebSearch != 0 || xSearch != 0 {
		t.Fatalf("summary tools=%d web=%d hosted=%d function=%d x=%d", tools, webSearch, hostedWebSearch, functionWebSearch, xSearch)
	}
}

func TestNormalizeHostedSearchDropsCollidingFunctionsAndDeduplicates(t *testing.T) {
	body := []byte(`{
		"model":"gpt-test",
		"tools":[
			{"type":"function","name":"web_search","parameters":{}},
			{"type":"function","name":"x_search","parameters":{}},
			{"type":"function","name":"save","parameters":{}},
			{"type":"x_search"},
			{"type":"web_search","filters":{"allowed_domains":["example.test"]}},
			{"type":"web_search"}
		],
		"tool_choice":{"type":"function","name":"web_search"}
	}`)
	adapted, changed, err := normalizeHostedSearchRequest(body, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var root map[string]any
	if err := json.Unmarshal(adapted, &root); err != nil {
		t.Fatal(err)
	}
	tools := anySlice(root["tools"])
	if len(tools) != 2 {
		t.Fatalf("tools=%d want hosted search + save: %s", len(tools), adapted)
	}
	if functionToolName(tools[0].(map[string]any)) != "save" || !isHostedWebSearchType(stringValue(tools[1].(map[string]any)["type"])) {
		t.Fatalf("wrong normalized tools: %s", adapted)
	}
	choice := root["tool_choice"].(map[string]any)
	if len(choice) != 1 || choice["type"] != "web_search" {
		t.Fatalf("tool choice still references a removed function: %s", adapted)
	}
	again, changedAgain, err := normalizeHostedSearchRequest(adapted, false)
	if err != nil || changedAgain || !bytes.Equal(again, adapted) {
		t.Fatalf("normalization must be idempotent: changed=%v err=%v body=%s", changedAgain, err, again)
	}
}

func TestNormalizeHostedSearchRequestRepairsDeepSeekHistory(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4-flash",
		"input":[
			{"type":"web_search_call","action":{"type":"search","query":"","sources":[]}},
			{"type":"web_search_call","action":{"type":"search","query":"DeepSeek news","sources":[]}},
			{"type":"web_search_call","action":{"type":"search","query":"ignored","queries":["preserved"]}},
			{"type":"web_search_call","action":{"type":"open_page","url":"https://example.test"}}
		],
		"tools":[{"type":"web_search"}]
	}`)
	adapted, changed, err := normalizeHostedSearchRequest(body, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}

	var root map[string]any
	if err := json.Unmarshal(adapted, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]any)
	first := input[0].(map[string]any)["action"].(map[string]any)
	if queries, ok := first["queries"].([]any); !ok || len(queries) != 0 {
		t.Fatalf("empty query must become an empty queries array: %s", adapted)
	}
	second := input[1].(map[string]any)["action"].(map[string]any)
	queries := second["queries"].([]any)
	if len(queries) != 1 || queries[0] != "DeepSeek news" {
		t.Fatalf("query was not preserved in queries: %s", adapted)
	}
	third := input[2].(map[string]any)["action"].(map[string]any)
	queries = third["queries"].([]any)
	if len(queries) != 1 || queries[0] != "preserved" {
		t.Fatalf("existing queries were overwritten: %s", adapted)
	}
	fourth := input[3].(map[string]any)["action"].(map[string]any)
	if _, exists := fourth["queries"]; exists {
		t.Fatalf("non-search action was modified: %s", adapted)
	}

	again, changedAgain, err := normalizeHostedSearchRequest(adapted, false)
	if err != nil || changedAgain || !bytes.Equal(again, adapted) {
		t.Fatalf("repair must be idempotent: changed=%v err=%v body=%s", changedAgain, err, again)
	}
}

func TestNormalizeHostedSearchRequestDoesNotPatchOtherModels(t *testing.T) {
	body := []byte(`{"model":"gpt-test","input":[{"type":"web_search_call","action":{"type":"search","query":""}}],"tools":[{"type":"web_search"}]}`)
	got, changed, err := normalizeHostedSearchRequest(body, false)
	if err != nil || changed || !bytes.Equal(got, body) {
		t.Fatalf("non-DeepSeek replay changed: changed=%v err=%v body=%s", changed, err, got)
	}
}

func TestNormalizeHostedSearchCollapsesXSearchForNonGrokRoute(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"function","name":"save","parameters":{}},
			{"type":"web_search","filters":{"allowed_domains":["example.test"]}},
			{"type":"x_search"}
		],
		"tool_choice":{"type":"allowed_tools","tools":[{"type":"web_search"},{"type":"x_search"}]}
	}`)
	adapted, changed, err := normalizeHostedSearchRequest(body, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var root map[string]any
	if err := json.Unmarshal(adapted, &root); err != nil {
		t.Fatal(err)
	}
	tools := anySlice(root["tools"])
	if len(tools) != 2 || stringValue(tools[1].(map[string]any)["type"]) != "web_search" {
		t.Fatalf("canonical web search missing: %s", adapted)
	}
	web := tools[1].(map[string]any)
	if filters, _ := web["filters"].(map[string]any); len(anySlice(filters["allowed_domains"])) != 1 {
		t.Fatalf("web_search options were lost: %s", adapted)
	}
	choice := root["tool_choice"].(map[string]any)
	allowed := anySlice(choice["tools"])
	if len(allowed) != 1 || stringValue(allowed[0].(map[string]any)["type"]) != "web_search" {
		t.Fatalf("allowed_tools was not aligned with Grok dialect: %s", adapted)
	}
	again, changedAgain, err := normalizeHostedSearchRequest(adapted, false)
	if err != nil || changedAgain || !bytes.Equal(again, adapted) {
		t.Fatalf("normalization must be idempotent: changed=%v err=%v body=%s", changedAgain, err, again)
	}
}

func TestNormalizeHostedSearchKeepsBuildPairForGrokRoute(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"function","name":"web_search","parameters":{}},
			{"type":"function","name":"save","parameters":{}},
			{"type":"x_search","from_date":"2026-01-01"},
			{"type":"web_search_preview","filters":{"allowed_domains":["example.test"]}},
			{"type":"x_search"}
		],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"web_search"}]}
	}`)
	adapted, changed, err := normalizeHostedSearchRequest(body, true)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var root map[string]any
	if err := json.Unmarshal(adapted, &root); err != nil {
		t.Fatal(err)
	}
	tools := anySlice(root["tools"])
	if len(tools) != 3 || functionToolName(tools[0].(map[string]any)) != "save" ||
		stringValue(tools[1].(map[string]any)["type"]) != "web_search" ||
		stringValue(tools[2].(map[string]any)["type"]) != "x_search" {
		t.Fatalf("Grok hosted tools were not normalized to Build order: %s", adapted)
	}
	web := tools[1].(map[string]any)
	if filters, _ := web["filters"].(map[string]any); len(anySlice(filters["allowed_domains"])) != 1 {
		t.Fatalf("web_search options were lost: %s", adapted)
	}
	xSearch := tools[2].(map[string]any)
	if stringValue(xSearch["from_date"]) != "2026-01-01" {
		t.Fatalf("x_search options were lost: %s", adapted)
	}
	choice := root["tool_choice"].(map[string]any)
	allowed := anySlice(choice["tools"])
	if len(allowed) != 2 || stringValue(allowed[0].(map[string]any)["type"]) != "web_search" ||
		stringValue(allowed[1].(map[string]any)["type"]) != "x_search" || choice["mode"] != "auto" {
		t.Fatalf("allowed_tools was not aligned with Build's Grok route: %s", adapted)
	}
	again, changedAgain, err := normalizeHostedSearchRequest(adapted, true)
	if err != nil || changedAgain || !bytes.Equal(again, adapted) {
		t.Fatalf("normalization must be idempotent: changed=%v err=%v body=%s", changedAgain, err, again)
	}
}

func TestSearchEvidenceCountsWithoutLoggingQueriesOrURLs(t *testing.T) {
	evidence := newSearchEvidence()
	evidence.observeJSON([]byte(`{
		"output":[
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"SECRET_QUERY","sources":[{"type":"url","url":"https://secret.example/a"},{"type":"url","url":"https://secret.example/b"}]}},
			{"type":"function_call","call_id":"fetch_1","name":"web_fetch","arguments":"{\"url\":\"https://secret.example/a\"}"},
			{"type":"message","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://secret.example/a"}]}]}
		],
		"usage":{"server_tool_use":{"web_search_requests":1}}
	}`))
	calls, completed, queries, sources, annotations, usage, fetches := evidence.counts()
	if calls != 1 || completed != 1 || queries != 1 || sources != 2 || annotations != 1 || usage != 1 || fetches != 1 {
		t.Fatalf("wrong evidence calls=%d completed=%d queries=%d sources=%d annotations=%d usage=%d fetches=%d",
			calls, completed, queries, sources, annotations, usage, fetches)
	}
	var logs bytes.Buffer
	server := New(log.New(&logs, "", 0))
	server.logSearchEvidence("channel", facadeRequest{HostedWebSearch: true}, evidence)
	if bytes.Contains(logs.Bytes(), []byte("SECRET")) || bytes.Contains(logs.Bytes(), []byte("secret.example")) {
		t.Fatalf("search diagnostics leaked content: %s", logs.String())
	}
}

func TestSearchSourcesCoverResponsesAndChatMetadataVariants(t *testing.T) {
	responses := map[string]any{
		"output": []any{
			webSearchItem("ws_1", "current news", nil, "completed"),
			messageItem("answer", nil),
		},
		"citations": []any{"https://responses.example/source"},
	}
	backfillResponseSearchSources(responses, true, "current news")
	if urls := urlsFromJSON(responses["output"]); len(urls) != 1 || urls[0] != "https://responses.example/source" {
		t.Fatalf("Responses top-level citations were not normalized: %#v", responses)
	}

	chatVariants := []struct {
		name     string
		metadata string
		wantURL  string
	}{
		{
			name: "openai_annotations",
			metadata: `"annotations":[{"type":"url_citation","url":"https://chat.example/annotation",` +
				`"title":"Source","start_index":0,"end_index":6}]`,
			wantURL: "https://chat.example/annotation",
		},
		{
			name:     "top_level_citations",
			metadata: `"citations":["https://chat.example/top"]`,
			wantURL:  "https://chat.example/top",
		},
		{
			name:     "web_search_results",
			metadata: `"web_search_results":[{"url":"https://chat.example/result"}]`,
			wantURL:  "https://chat.example/result",
		},
	}
	for _, test := range chatVariants {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"answer",` +
				test.metadata + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			result, err := canonicalFromChat(body, true, "current news")
			if err != nil {
				t.Fatal(err)
			}
			if urls := urlsFromJSON(result.Output); len(urls) != 1 || urls[0] != test.wantURL {
				t.Fatalf("Chat source metadata was not normalized: %#v", result.Output)
			}
		})
	}
}

func TestReadBodyLimitedDetectsOverflow(t *testing.T) {
	data, err := readBodyLimited(strings.NewReader("1234"), 4)
	if err != nil || string(data) != "1234" {
		t.Fatalf("exact limit: data=%q err=%v", data, err)
	}
	if _, err := readBodyLimited(strings.NewReader("12345"), 4); !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestDecodeJSONMapRejectsTrailingContent(t *testing.T) {
	if _, err := decodeJSONMap([]byte(`{"choices":[]} {"extra":true}`)); err == nil {
		t.Fatal("multiple JSON values were accepted")
	}
}
