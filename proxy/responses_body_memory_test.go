package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestResponsesWebSocketTurnPreparationPreservesBodyAndIntent(t *testing.T) {
	for _, raw := range []string{
		`{"model":"gpt-5.5","input":"explain this error","store":false}`,
		`{"model":"gpt-5.5","prompt":"draw a cat","input":"explain this error"}`,
		`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"draw a cat"}]}]}`,
		`{"model":"gpt-5.5","previous_response_id":"resp_previous","input":[{"type":"function_call_output","call_id":"call_one","output":"ok"}]}`,
		`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"},{"role":"user","content":"continue"}]}`,
		`{"model":"gpt-5.5","input":"hello","tools":[{"type":"namespace","name":"image_gen","tools":[]}]}`,
	} {
		body := []byte(raw)
		public, input := PrepareResponsesWebSocketBody(body)
		turn, intent := prepareResponsesWebSocketTurnBody(body)
		if !bytes.Equal(public, turn) {
			t.Fatalf("native turn changed prepared body for %s", raw)
		}
		if input != gjson.GetBytes(public, "input").Raw {
			t.Fatal("public preparation lost its replay input result")
		}
		if intent != responsesBodyHasNaturalImageGenerationIntent(body) {
			t.Fatalf("image intent changed before transport selection for %s", raw)
		}
	}
}

func TestNormalizeImageIntentTextPreservesWhitespaceAndPunctuation(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"  Draw,\tA\nCAT!!! ", "draw a cat"},
		{"，画一张。猫；", "画一张 猫"},
		{"draw\u00a0\u2003a  cat", "draw a cat"},
		{"draw 'a' `cat`", "draw a cat"},
		{"...", ""},
		{"HELLO ÉCOLE Σ", "hello école σ"},
		{"a\xff,b", "a� b"},
		{"already normalized", "already normalized"},
	} {
		if got := normalizeImageIntentText(tc.input); got != tc.want {
			t.Fatalf("normalize(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestStripResponsesInputImageToolsKeepsUnchangedHistoryOpaque(t *testing.T) {
	for _, carrier := range []string{
		"",
		`,{"type":"additional_tools","tools":[{"type":"function","name":"read"}]}`,
		`,{"type":"other_additional_tools_type","tools":[{"type":"image_generation"}]}`,
	} {
		body := []byte(`{"input":[{"type":"message","content":"` + strings.Repeat("a", 1<<20) + `","large":9007199254740993}` + carrier + `]}`)
		out := stripResponsesImageGenerationCapabilities(body)
		if !sameRequestBodyBuffer(body, out) {
			t.Fatal("unchanged history was rebuilt")
		}
	}

	original := `{"type":"message","large":9007199254740993,"content":"keep\\raw"}`
	body := []byte(`{"input":[` + original + `,{"type":" \u0061dditional_tools ","tools":[{"type":"image_generation"}]},{"type":"additional_tools","tools":[{"type":"function","name":"read"},{"type":"image_generation"}]}]}`)
	out := stripResponsesImageGenerationCapabilities(body)
	var decoded struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Input) != 2 || string(decoded.Input[0]) != original {
		t.Fatalf("ordinary history was changed when stripping another item: %s", out)
	}
	if got := gjson.GetBytes(decoded.Input[1], "tools.#").Int(); got != 1 {
		t.Fatalf("remaining carrier has %d tools, want 1", got)
	}
}
