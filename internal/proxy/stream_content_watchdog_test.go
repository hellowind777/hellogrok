package proxy

import (
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestContentWatchdogTimeout(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  time.Duration
	}{
		{
			name:  "unconfigured falls back to Grok Build default minus margin",
			route: config.Route{},
			want:  600*time.Second - contentWatchdogMargin,
		},
		{
			name: "managed 1800s minus margin",
			route: config.Route{
				InferenceIdleTimeoutSecs:       1800,
				InferenceIdleTimeoutConfigured: true,
			},
			want: 1800*time.Second - contentWatchdogMargin,
		},
		{
			name: "configured value below floor disables the watchdog",
			route: config.Route{
				InferenceIdleTimeoutSecs:       60,
				InferenceIdleTimeoutConfigured: true,
			},
			want: 0,
		},
		{
			name: "smallest enabled timeout",
			route: config.Route{
				InferenceIdleTimeoutSecs:       90,
				InferenceIdleTimeoutConfigured: true,
			},
			want: 60 * time.Second,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := contentWatchdogTimeout(test.route); got != test.want {
				t.Fatalf("contentWatchdogTimeout() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestContentWatchdogFiresOnceAndStops(t *testing.T) {
	fired := make(chan struct{}, 1)
	watchdog := &contentWatchdog{timeout: 20 * time.Millisecond, onFire: func() { fired <- struct{}{} }}
	watchdog.timer = time.AfterFunc(watchdog.timeout, watchdog.fire)

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}
	if !watchdog.stalled() {
		t.Fatal("stalled() should report the fired watchdog")
	}
	watchdog.content() // must not resurrect a fired watchdog
	watchdog.stop()
	select {
	case <-fired:
		t.Fatal("watchdog fired twice")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestContentWatchdogContentResetsTimer(t *testing.T) {
	fired := make(chan struct{}, 1)
	watchdog := &contentWatchdog{timeout: 60 * time.Millisecond, onFire: func() { fired <- struct{}{} }}
	watchdog.timer = time.AfterFunc(watchdog.timeout, watchdog.fire)
	defer watchdog.stop()

	for i := 0; i < 3; i++ {
		time.Sleep(30 * time.Millisecond)
		watchdog.content()
	}
	select {
	case <-fired:
		t.Fatal("watchdog fired despite content resets")
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire after content stopped")
	}
}

func TestChatFrameHasContent(t *testing.T) {
	tests := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{
			name: "text delta",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "hello"},
			}}},
			want: true,
		},
		{
			name: "reasoning delta",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"reasoning_content": "thinking"},
			}}},
			want: true,
		},
		{
			name: "tool call delta",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0}}},
			}}},
			want: true,
		},
		{
			name: "finish reason",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}}},
			want: true,
		},
		{
			name:  "error frame",
			frame: map[string]any{"error": map[string]any{"message": "boom"}},
			want:  true,
		},
		{
			name: "empty delta keepalive does not count",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{},
			}}},
			want: false,
		},
		{
			name: "empty string deltas do not count",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta": map[string]any{"content": "", "reasoning_content": ""},
			}}},
			want: false,
		},
		{
			name: "usage-only chunk does not count",
			frame: map[string]any{
				"choices": []any{},
				"usage":   map[string]any{"prompt_tokens": 10},
			},
			want: false,
		},
		{
			name: "null finish reason does not count",
			frame: map[string]any{"choices": []any{map[string]any{
				"delta":         map[string]any{"content": ""},
				"finish_reason": nil,
			}}},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := chatFrameHasContent(test.frame); got != test.want {
				t.Fatalf("chatFrameHasContent() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestMessagesFrameHasContent(t *testing.T) {
	if messagesFrameHasContent(map[string]any{"type": "ping"}) {
		t.Fatal("ping must not count as content")
	}
	for _, typ := range []string{"message_start", "message_delta", "message_stop", "content_block_start", "content_block_delta", "content_block_stop", "error"} {
		if !messagesFrameHasContent(map[string]any{"type": typ}) {
			t.Fatalf("%s must count as content", typ)
		}
	}
	if messagesFrameHasContent(map[string]any{}) {
		t.Fatal("missing type must not count as content")
	}
}

func TestResponsesFrameHasContent(t *testing.T) {
	tests := []struct {
		name  string
		frame map[string]any
		want  bool
	}{
		{name: "created does not count", frame: map[string]any{"type": "response.created"}, want: false},
		{name: "in_progress does not count", frame: map[string]any{"type": "response.in_progress"}, want: false},
		{name: "queued does not count", frame: map[string]any{"type": "response.queued"}, want: false},
		{name: "output text delta", frame: map[string]any{"type": "response.output_text.delta", "delta": "hi"}, want: true},
		{name: "empty output text delta", frame: map[string]any{"type": "response.output_text.delta", "delta": ""}, want: false},
		{name: "reasoning delta", frame: map[string]any{"type": "response.reasoning_text.delta", "delta": "why"}, want: true},
		{name: "function call arguments delta", frame: map[string]any{"type": "response.function_call_arguments.delta", "delta": "{"}, want: true},
		{name: "empty function call arguments done without name", frame: map[string]any{"type": "response.function_call_arguments.done", "arguments": ""}, want: false},
		{name: "function call arguments done with name", frame: map[string]any{"type": "response.function_call_arguments.done", "arguments": "", "name": "read_file"}, want: true},
		{name: "output item added counts", frame: map[string]any{"type": "response.output_item.added"}, want: true},
		{name: "completed counts", frame: map[string]any{"type": "response.completed"}, want: true},
		{name: "failed counts", frame: map[string]any{"type": "response.failed"}, want: true},
		{name: "web search in progress counts", frame: map[string]any{"type": "response.web_search_call.in_progress"}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := responsesFrameHasContent(test.frame); got != test.want {
				t.Fatalf("responsesFrameHasContent() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestContentWatchdogDisabledForSmallTimeouts(t *testing.T) {
	fired := false
	watchdog := newContentWatchdog(config.Route{
		InferenceIdleTimeoutSecs:       30,
		InferenceIdleTimeoutConfigured: true,
	}, func() { fired = true })
	if watchdog != nil {
		t.Fatal("watchdog must be disabled for tiny configured timeouts")
	}
	if fired {
		t.Fatal("disabled watchdog fired")
	}
}
