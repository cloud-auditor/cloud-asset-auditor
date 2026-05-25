package output

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// JSON renders assets as a single JSON array (default) or as NDJSON when
// Stream is true. Both modes drain the channel incrementally so memory
// stays bounded against large inventories.
type JSON struct {
	Stream bool
}

func (r *JSON) Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush() //nolint:errcheck // best-effort on panic; natural-exit paths flush explicitly.

	if r.Stream {
		return r.renderNDJSON(ctx, in, bw)
	}
	return r.renderArray(ctx, in, bw)
}

func (r *JSON) renderNDJSON(ctx context.Context, in <-chan core.Asset, bw *bufio.Writer) error {
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case a, ok := <-in:
			if !ok {
				return bw.Flush()
			}
			if err := enc.Encode(a); err != nil {
				return fmt.Errorf("encode asset: %w", err)
			}
		}
	}
}

func (r *JSON) renderArray(ctx context.Context, in <-chan core.Asset, bw *bufio.Writer) error {
	if err := bw.WriteByte('['); err != nil {
		return err
	}
	first := true
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case a, ok := <-in:
			if !ok {
				if _, err := bw.WriteString("]\n"); err != nil {
					return err
				}
				return bw.Flush()
			}
			if !first {
				if err := bw.WriteByte(','); err != nil {
					return err
				}
			}
			first = false
			b, err := marshalCompact(a)
			if err != nil {
				return fmt.Errorf("encode asset: %w", err)
			}
			if _, err := bw.Write(b); err != nil {
				return err
			}
		}
	}
}

// marshalCompact encodes a with HTML escaping off and no trailing newline,
// suitable for array-mode framing.
func marshalCompact(a core.Asset) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(a); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
