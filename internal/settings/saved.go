package settings

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"

	"github.com/devvmhq/devvm/internal/msg"
	"github.com/devvmhq/devvm/internal/state"
)

// readSaved reads settings.json at path. A missing file sets nothing; a
// field devvm doesn't know is ignored, and one devvm can't use is a
// state.CorruptError naming the file and the field.
func readSaved(path string) (Values, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths come from paths.Dirs
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Values{}, nil
	case err != nil:
		return Values{}, err
	}
	var v Values
	if err := json.Unmarshal(data, &v); err != nil {
		return Values{}, &state.CorruptError{Path: path, Reason: err.Error()}
	}
	for k := range v.Retention {
		if !slices.Contains(RetentionKeys(), k) {
			delete(v.Retention, k)
		}
	}
	if key, err := v.check(false); err != nil {
		c := &state.CorruptError{Path: path, Reason: key + ": " + err.Error()}
		if m, ok := errors.AsType[msg.M](err); ok {
			c.Reason, c.Msg = key+": ", m
		}
		return Values{}, c
	}
	return v, nil
}

// writeSaved replaces settings.json at path with v, indented.
func writeSaved(path string, v Values) error {
	data, _ := json.MarshalIndent(v, "", "  ")
	return state.WriteFile(path, append(data, '\n'))
}
