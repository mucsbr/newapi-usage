package audit

import (
	"bytes"
	"compress/gzip"
	"io"
)

const bodyEncodingGzip = "gzip"

func encodeBody(body string) ([]byte, string, error) {
	if body == "" {
		return nil, "", nil
	}
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write([]byte(body)); err != nil {
		_ = writer.Close()
		return nil, "", err
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), bodyEncodingGzip, nil
}

func decodeBody(raw string, encoded []byte, encoding string) (string, error) {
	if encoding != bodyEncodingGzip || len(encoded) == 0 {
		return raw, nil
	}
	reader, err := gzip.NewReader(bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
