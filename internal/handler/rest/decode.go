package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxBodyBytes limits the size of a JSON request body.
const maxBodyBytes = 1 << 20

// decodeJSON reads exactly one JSON value with only known fields into dst. The
// returned error is a *requestError that says what is wrong with the body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &requestError{
			status:  http.StatusUnsupportedMediaType,
			code:    codeUnsupportedMediaType,
			message: "Content-Type must be application/json",
		}
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return mapDecodeError(err)
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return mapDecodeError(err)
		}

		return invalidJSON("request body must contain a single JSON value")
	}

	return nil
}

func mapDecodeError(err error) error {
	var (
		tooLarge  *http.MaxBytesError
		syntax    *json.SyntaxError
		wrongType *json.UnmarshalTypeError
	)

	switch {
	case errors.As(err, &tooLarge):
		return &requestError{
			status:  http.StatusRequestEntityTooLarge,
			code:    codePayloadTooLarge,
			message: fmt.Sprintf("request body must not exceed %d bytes", tooLarge.Limit),
		}
	case errors.Is(err, io.EOF):
		return invalidJSON("request body is empty")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return invalidJSON("request body is not valid JSON")
	case errors.As(err, &wrongType):
		return invalidJSON(fmt.Sprintf("field %q has the wrong type or is out of range: got %s", wrongType.Field, wrongType.Value))
	}

	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return invalidJSON("unknown field " + name)
	}

	// What remains are failures of a field's own decoder; the only such fields
	// in this API are timestamps.
	return invalidJSON("timestamps must be in RFC 3339 format, for example 2030-01-31T19:00:00Z")
}

func invalidJSON(message string) error {
	return &requestError{status: http.StatusBadRequest, code: codeInvalidJSON, message: message}
}
