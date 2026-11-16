package cnpg

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/Nortezh/api"
)

func validateRetentionCatalog(data []byte, it *api.DeployerCommandCNPGBackupRetention) error {
	invalid := errors.New("CNPG retention catalog unavailable or mismatched")
	if len(data) == 0 || len(data) > retentionLogLimit {
		return invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if uniqueJSONValue(decoder, 0) != nil {
		return invalid
	}
	if _, err := decoder.Token(); err != io.EOF {
		return invalid
	}
	var catalog struct {
		Backups []struct {
			ID     string `json:"backup_id"`
			Status string `json:"status"`
		} `json:"backups_list"`
	}
	if json.Unmarshal(data, &catalog) != nil || len(catalog.Backups) != 7 {
		return invalid
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil || len(root) != 1 {
		return invalid
	}
	expected := make(map[string]bool, 7)
	for _, b := range it.Backups[1:] {
		expected[b.Material.BarmanBackupID] = true
	}
	for _, b := range catalog.Backups {
		if b.Status != "DONE" || !expected[b.ID] {
			return invalid
		}
		delete(expected, b.ID)
	}
	if len(expected) != 0 {
		return invalid
	}
	return nil
}

// The standard decoder accepts duplicate object keys; reject those before decoding evidence.
func uniqueJSONValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("catalog too deep")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return errors.New("invalid catalog delimiter")
	}
	keys := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate catalog key")
			}
			keys[name] = true
		}
		if err := uniqueJSONValue(d, depth+1); err != nil {
			return err
		}
	}
	end, err := d.Token()
	if err != nil {
		return err
	}
	if delim == '{' && end != json.Delim('}') || delim == '[' && end != json.Delim(']') {
		return errors.New("invalid catalog close")
	}
	return nil
}
