package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const maxAliasList = 1 << 20

func modelListRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/models"
}

type modelListBody struct {
	io.Reader
	io.Closer
}

func (g *Gateway) advertiseModelAlias(resp *http.Response, info *reqInfo) error {
	resp.Header.Set("Cache-Control", "no-store")
	encoding := resp.Header.Get("Content-Encoding")
	if encoding != "" && !strings.EqualFold(encoding, "identity") ||
		strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.ContentLength > maxAliasList {
		return nil
	}
	// Reserve room for the input, decoded records and rewritten reply.
	const reserve = 4 * maxAliasList
	if !g.buffered.take(reserve) {
		return errBusy
	}
	info.listBudget = reserve
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAliasList+1))
	if err != nil {
		return err
	}
	// If the reply is too large or cannot be rewritten, replay the bytes
	// already read before streaming the rest of the original body.
	if len(body) <= maxAliasList {
		if updated := addAliasToModelList(body, info.t.defaultModel); updated != nil {
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(updated))
			resp.ContentLength = int64(len(updated))
			resp.Header.Set("Content-Length", strconv.Itoa(len(updated)))
			for _, header := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
				resp.Header.Del(header)
			}
			resp.Trailer = nil
			return nil
		}
	}
	resp.Body = modelListBody{Reader: io.MultiReader(bytes.NewReader(body), resp.Body), Closer: resp.Body}
	return nil
}

// addAliasToModelList copies the target's record and keeps upstream fields,
// including pagination cursors, intact. The configured alias owns its id.
func addAliasToModelList(body []byte, target string) []byte {
	var list map[string]json.RawMessage
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var records []json.RawMessage
	if json.Unmarshal(list["data"], &records) != nil || records == nil {
		return nil
	}
	models := make([]json.RawMessage, 0, len(records)+1)
	var alias json.RawMessage
	var removed bool
	for _, record := range records {
		var model struct{ ID string }
		if json.Unmarshal(record, &model) != nil || model.ID == "" {
			return nil
		}
		if model.ID == ModelAlias {
			removed = true
			continue
		}
		models = append(models, record)
		if model.ID == target && alias == nil {
			var fields map[string]json.RawMessage
			if json.Unmarshal(record, &fields) != nil {
				return nil
			}
			fields["id"], _ = json.Marshal(ModelAlias)
			for _, key := range []string{"name", "display_name"} {
				if _, exists := fields[key]; exists {
					fields[key] = fields["id"]
				}
			}
			alias, _ = json.Marshal(fields)
		}
	}
	if alias != nil {
		models = append(models, alias)
	} else if !removed {
		return nil
	}
	list["data"], _ = json.Marshal(models)
	updated, _ := json.Marshal(list)
	return updated
}
