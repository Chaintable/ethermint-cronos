package util

import (
	"bytes"
	"compress/gzip"
	"encoding/json"

	"github.com/ethereum/go-ethereum/rlp"
)

func EncodeToJsonGzip(v any) ([]byte, error) {
	jsonBytes, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(jsonBytes); err != nil {
		zw.Close()
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func DecodeFromGzipJson(v []byte, target any) error {
	gz, err := gzip.NewReader(bytes.NewBuffer(v))
	if err != nil {
		return err
	}
	return json.NewDecoder(gz).Decode(target)
}

func EncodeToRlp(v any) ([]byte, error) {
	return rlp.EncodeToBytes(v)
}

func DecodeFromRlp(v []byte, target any) error {
	return rlp.DecodeBytes(v, target)
}
