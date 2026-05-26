package logging_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/logging"
)

func TestNew_DefaultsToTextInfoStderr(t *testing.T) {
	// Empty Options should produce a working logger that filters DEBUG
	// but emits INFO+.
	var buf bytes.Buffer
	l, err := logging.New(logging.Options{Output: &buf})
	if err != nil {
		t.Fatal(err)
	}

	l.Debug("should be filtered")
	l.Info("should appear", "key", "value")
	l.Warn("should appear too")

	got := buf.String()
	if strings.Contains(got, "should be filtered") {
		t.Errorf("DEBUG line leaked at default INFO level:\n%s", got)
	}
	if !strings.Contains(got, "should appear") {
		t.Errorf("INFO line missing:\n%s", got)
	}
	if !strings.Contains(got, "should appear too") {
		t.Errorf("WARN line missing:\n%s", got)
	}
	// Text handler emits key=value pairs.
	if !strings.Contains(got, "key=value") {
		t.Errorf("text handler should emit `key=value` form:\n%s", got)
	}
}

func TestNew_JSONFormat(t *testing.T) {
	var buf bytes.Buffer
	l, err := logging.New(logging.Options{Format: "json", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hi", "n", 42)

	// Each line should be a parseable JSON object.
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("non-JSON line %q in JSON-format output: %v", line, err)
		}
		if obj["msg"] != "hi" || obj["n"].(float64) != 42 {
			t.Errorf("missing or wrong fields in JSON record: %v", obj)
		}
	}
}

func TestNew_LevelFiltering(t *testing.T) {
	cases := []struct {
		level     string
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
		wantError bool
	}{
		{"debug", true, true, true, true},
		{"info", false, true, true, true},
		{"warn", false, false, true, true},
		{"error", false, false, false, true},
	}
	for _, c := range cases {
		t.Run(c.level, func(t *testing.T) {
			var buf bytes.Buffer
			l, err := logging.New(logging.Options{Level: c.level, Output: &buf})
			if err != nil {
				t.Fatal(err)
			}
			l.Debug("d")
			l.Info("i")
			l.Warn("w")
			l.Error("e")
			out := buf.String()
			if has := strings.Contains(out, `"d"`) || strings.Contains(out, "msg=d"); has != c.wantDebug {
				t.Errorf("debug emitted=%v want=%v\noutput: %s", has, c.wantDebug, out)
			}
			if has := strings.Contains(out, `"i"`) || strings.Contains(out, "msg=i"); has != c.wantInfo {
				t.Errorf("info emitted=%v want=%v", has, c.wantInfo)
			}
			if has := strings.Contains(out, `"w"`) || strings.Contains(out, "msg=w"); has != c.wantWarn {
				t.Errorf("warn emitted=%v want=%v", has, c.wantWarn)
			}
			if has := strings.Contains(out, `"e"`) || strings.Contains(out, "msg=e"); has != c.wantError {
				t.Errorf("error emitted=%v want=%v", has, c.wantError)
			}
		})
	}
}

func TestNew_UnknownLevelErrors(t *testing.T) {
	if _, err := logging.New(logging.Options{Level: "loud"}); err == nil {
		t.Error("expected error for unknown level")
	}
}

func TestNew_UnknownFormatFallsBackToText(t *testing.T) {
	// Format typos must NOT crash the binary — production operators
	// who pass "JSON" (capitalized) or "yaml" by mistake get text logs,
	// not a startup failure.
	var buf bytes.Buffer
	l, err := logging.New(logging.Options{Format: "yaml", Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hi", "k", "v")
	if !strings.Contains(buf.String(), "k=v") {
		t.Errorf("unknown format should fall back to text:\n%s", buf.String())
	}
}

func TestSetDefault_AffectsPackageLevelCalls(t *testing.T) {
	// Save and restore the slog default — other tests in this package
	// must not see the swap.
	prev := slog.Default()
	defer slog.SetDefault(prev)

	var buf bytes.Buffer
	l, err := logging.New(logging.Options{Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	logging.SetDefault(l)

	slog.Info("from package level", "ctx", "default")
	if !strings.Contains(buf.String(), "from package level") {
		t.Errorf("SetDefault should reroute slog.Info to our handler\n%s", buf.String())
	}
}

func TestNew_AddSourceIncludesFileLine(t *testing.T) {
	var buf bytes.Buffer
	l, err := logging.New(logging.Options{Format: "json", AddSource: true, Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("here")

	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	src, ok := obj["source"].(map[string]any)
	if !ok {
		t.Fatalf("source field missing or wrong type: %v", obj)
	}
	if _, ok := src["file"].(string); !ok {
		t.Errorf("source.file missing: %v", src)
	}
}
