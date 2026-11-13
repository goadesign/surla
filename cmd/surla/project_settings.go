// This file implements stopped-or-running project management of startup verification and promotion settings.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/project"
)

// settings validates a named operator file before atomically storing its normalized copy.
// Relative local repository and release paths are resolved against the named file's directory.
func (runtime projectRuntime) settings(kind string, args []string, stdout io.Writer) (err error) {
	if len(args) == 1 && slices.Contains(helpWords, args[0]) {
		_, err := fmt.Fprintf(os.Stderr, "Usage: surla %s set <file> | show | remove\n", kind)
		return err
	}
	valid := len(args) == 2 && args[0] == "set" || len(args) == 1 && (args[0] == "show" || args[0] == "remove")
	if !valid {
		return fmt.Errorf("usage: surla %s set <file> | show | remove", kind)
	}
	state, err := runtime.discover()
	if err != nil {
		return err
	}
	if state.Record.ID == "" && args[0] != "set" {
		_, err := fmt.Fprintf(stdout, "No %s settings are stored.\n", kind)
		return err
	}
	var snapshot []byte
	if args[0] == "set" {
		path, err := abspath.Eval(args[1])
		if err != nil {
			return err
		}
		snapshot, err = validatedSettings(kind, path, state)
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lock, err := state.StartLock(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	created, err := state.Initialize(fmt.Sprintf("uid:%d", os.Geteuid()))
	if err != nil {
		return err
	}
	if created {
		if err := writeProjectCreatedNotice(stdout); err != nil {
			return err
		}
	}
	// Only local CLI management reads this proof; no agent tool receives it.
	if _, err := state.ManagementCredential(); err != nil {
		return err
	}
	switch args[0] {
	case "show":
		raw, err := state.ReadSettings(kind)
		if errors.Is(err, os.ErrNotExist) {
			_, err = fmt.Fprintf(stdout, "No %s settings are stored.\n", kind)
			return err
		}
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s\n", raw)
		return err
	case "remove":
		if err := state.RemoveSettings(kind); err != nil {
			return err
		}
	case "set":
		if err := state.SaveSettings(kind, snapshot); err != nil {
			return err
		}
	}
	verb := "stored"
	if args[0] == "remove" {
		verb = "removed"
	}
	_, err = fmt.Fprintf(stdout, "%s settings %s. Changes take effect the next time this project's server starts.\n", kind, verb)
	return err
}

// validatedSettings reuses the owning loaders and rejects removed storage fields before any persistence.
func validatedSettings(kind, path string, state *project.State) ([]byte, error) {
	protected, err := workspaceProtectedDirs(state.Record.Directory)
	if err != nil {
		return nil, err
	}
	if err := validateWorkspaceDirSeparation("settings file", path, protected...); err != nil {
		return nil, err
	}
	switch kind {
	case "verification":
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file verificationFile
		if err := decodeSettings(raw, &file); err != nil {
			return nil, err
		}
		cfg, err := verificationConfig(file, "", "")
		if err != nil {
			return nil, err
		}

		file.CandidateRepo, err = resolveRepositorySetting(filepath.Dir(path), cfg.Plan.CandidateRepo)
		if err != nil {
			return nil, err
		}
		file.SuiteRepo, err = resolveRepositorySetting(filepath.Dir(path), cfg.Plan.SuiteRepo)
		if err != nil {
			return nil, err
		}
		return json.Marshal(file)
	case "promotion":
		file, err := decodePromotionFileAtBase(path, filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		file.ReleaseDir, err = abspath.EvalMissing(file.ReleaseDir)
		if err != nil {
			return nil, err
		}
		if err := validateWorkspaceDirSeparation("release_dir", file.ReleaseDir, protected...); err != nil {
			return nil, err
		}
		return json.Marshal(file)
	default:
		return nil, errors.New("unknown settings")
	}
}

// resolveRepositorySetting preserves remote Git addresses and resolves local path spellings before storing them.
func resolveRepositorySetting(base, repository string) (string, error) {
	if strings.Contains(repository, ":") {
		return repository, nil
	}
	return abspath.EvalFrom(base, repository)
}

// decodeSettings applies the same strict current-format boundary to persisted settings and operator files.
func decodeSettings(raw []byte, value any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("data after the configuration")
	}
	return nil
}
