package executor

import (
	"strings"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// preserveCodexPreviousResponseID keeps the client's incremental Responses
// chain when the translator would drop it. This is a plus/OpenCode behavior.
func preserveCodexPreviousResponseID(from sdktranslator.Format, original, translated []byte) []byte {
	if !sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		return translated
	}
	prev := strings.TrimSpace(gjson.GetBytes(original, "previous_response_id").String())
	if prev == "" || gjson.GetBytes(translated, "previous_response_id").Exists() {
		return translated
	}
	updated, errSet := sjson.SetBytes(translated, "previous_response_id", prev)
	if errSet != nil {
		return translated
	}
	return updated
}

// normalizeCodexPreviousResponseIDForPromptCache drops the chain only when the
// client already sent a full transcript or compaction summary.
func normalizeCodexPreviousResponseIDForPromptCache(from sdktranslator.Format, rawJSON []byte) []byte {
	if !sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		return rawJSON
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) == "" {
		return rawJSON
	}
	input := gjson.GetBytes(rawJSON, "input")
	if !codexInputLooksLikeFullTranscript(input) {
		return rawJSON
	}
	updated, errDelete := sjson.DeleteBytes(rawJSON, "previous_response_id")
	if errDelete != nil {
		return rawJSON
	}
	return updated
}

func codexInputLooksLikeFullTranscript(input gjson.Result) bool {
	if !input.Exists() || !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("role").String()) == "assistant" {
			return true
		}
		switch strings.TrimSpace(item.Get("type").String()) {
		case "compaction", "compaction_summary":
			return true
		}
	}
	return false
}
