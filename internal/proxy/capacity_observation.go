package proxy

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/hellowind777/hellogrok/internal/capacity"
)

func completionLimitFromRequest(body []byte, protocol wireProtocol) uint64 {
	root, err := decodeRequestObject(body)
	if err != nil {
		return 0
	}
	key := "max_tokens"
	if protocol == wireResponses {
		key = "max_output_tokens"
	}
	value, _ := positiveJSONUint64(root[key])
	return value
}

func capacityObservationFromHeaders(header http.Header) capacity.Observation {
	observation := capacity.Observation{}
	if value, ok := positiveCapacityHeader(header.Get(grokContextWindowHeader), 64); ok {
		observation.ContextWindow = value
		observation.ContextSource = capacity.SourceResponseHeader
	}
	if value, ok := positiveCapacityHeader(header.Get(grokMaxCompletionTokensHeader), 32); ok {
		observation.MaxCompletionTokens = value
		observation.CompletionSource = capacity.SourceResponseHeader
	}
	return observation
}

func positiveCapacityHeader(value string, bitSize int) (uint64, bool) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, bitSize)
	return parsed, err == nil && parsed > 0
}
