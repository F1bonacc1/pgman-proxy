// JSON encoding helpers shared by every handler.

package control

import (
	"encoding/json"
	"io"
	"net/http"
)

// encodeJSON writes v as a JSON object terminated by a newline. Errors
// from the underlying ResponseWriter are swallowed — the connection is
// already broken at that point and there's nothing useful to do.
func encodeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// maxRequestBodyBytes caps inbound LCM bodies at 64 KiB. Every
// documented body is far smaller; an oversize request is almost
// certainly client-side abuse.
const maxRequestBodyBytes = 64 << 10

// decodeJSON reads up to maxRequestBodyBytes from r.Body into out.
// Returns the decoder error verbatim so handlers can surface a
// `invalid_argument` envelope without inventing a wrapper.
func decodeJSON(r *http.Request, out any) error {
	body := http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}
