package studiolocal

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/test-mesh/core/core/runner"
	"gopkg.in/yaml.v3"
)

//go:embed workspace.html
var workspacePage string

const maxSource = 2 << 20

type operation struct {
	Action   string
	Path     string
	Source   string
	Revision string
	EnvFile  string
}

type scalarField struct {
	Label  string `json:"label"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Value  string `json:"value"`
}

type result struct {
	Run      *runner.InlineResult `json:"run,omitempty"`
	Fields   []scalarField        `json:"fields,omitempty"`
	Source   string               `json:"source"`
	Revision string               `json:"revision,omitempty"`
	Output   string               `json:"output,omitempty"`
	Failed   bool                 `json:"failed,omitempty"`
}

func Serve(ctx context.Context, workspace string, output io.Writer) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	workspace, err = filepath.Abs(workspace)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	host := listener.Addr().String()
	origin := "http://" + host
	var workspaceLock sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, workspacePage)
	})
	mux.HandleFunc("POST /workspace", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != origin || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "Invalid request origin or content type", http.StatusForbidden)
			return
		}
		var op operation
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*maxSource)).Decode(&op); err != nil || len(op.Source) > maxSource {
			http.Error(w, "Invalid workspace request", http.StatusBadRequest)
			return
		}
		switch strings.ToLower(filepath.Ext(op.Path)) {
		case ".yaml", ".yml":
		default:
			http.Error(w, "Select a YAML file in the workspace", http.StatusBadRequest)
			return
		}
		workspaceLock.Lock()
		response, err := apply(r.Context(), root, workspace, executable, op)
		workspaceLock.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != host {
				http.Error(w, "Invalid host", http.StatusForbidden)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			mux.ServeHTTP(w, r)
		}),
	}
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	fmt.Fprintf(output, "Studio Free is listening at %s\n", origin)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func apply(ctx context.Context, root *os.Root, workspace, executable string, op operation) (result, error) {
	file, err := root.Open(op.Path)
	if err != nil {
		return result{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSource {
		return result{}, errors.New("Select a regular YAML file under 2 MiB")
	}
	source, err := io.ReadAll(io.LimitReader(file, maxSource+1))
	if err != nil || len(source) > maxSource {
		return result{}, errors.New("Could not read YAML file within the size limit")
	}
	revision := fmt.Sprintf("%x", sha256.Sum256(source))
	switch op.Action {
	case "open":
		return result{Source: string(source), Revision: revision, Fields: editableFields(source)}, nil
	case "save", "run":
		if revision != op.Revision {
			return result{}, errors.New("The file changed on disk. Reload it before saving or running.")
		}
		if op.Action == "run" && op.Source != string(source) {
			return result{}, errors.New("Save your changes before running.")
		}
	case "validate":
	default:
		return result{}, errors.New("Unknown workspace action")
	}

	validationFile, err := os.CreateTemp("", "testmesh-studio-*.yaml")
	if err != nil {
		return result{}, err
	}
	defer os.Remove(validationFile.Name())
	_, writeErr := io.WriteString(validationFile, op.Source)
	if err := errors.Join(writeErr, validationFile.Close()); err != nil {
		return result{}, err
	}
	validation := invoke(ctx, workspace, executable, "validate", validationFile.Name())
	if validation.Failed || op.Action == "validate" {
		return validation, nil
	}

	// Stage the replacement beside the original and rename inside the open root.
	staging := filepath.Join(filepath.Dir(op.Path), ".testmesh-"+filepath.Base(validationFile.Name()))
	next, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return result{}, err
	}
	defer root.Remove(staging)
	_, writeErr = io.WriteString(next, op.Source)
	if err := errors.Join(writeErr, next.Close()); err != nil {
		return result{}, err
	}
	if op.Action == "run" {
		args := []string{"run", "--json"}
		if op.EnvFile != "" {
			env, err := root.Open(op.EnvFile)
			if err != nil {
				return result{}, fmt.Errorf("open environment file: %w", err)
			}
			defer env.Close()
			info, err := env.Stat()
			if err != nil || !info.Mode().IsRegular() || info.Size() > maxSource {
				return result{}, errors.New("Select a regular environment file under 2 MiB")
			}
			snapshot, err := os.CreateTemp("", "testmesh-studio-env-*")
			if err != nil {
				return result{}, err
			}
			defer os.Remove(snapshot.Name())
			size, copyErr := io.Copy(snapshot, io.LimitReader(env, maxSource+1))
			if err := errors.Join(copyErr, snapshot.Close()); err != nil {
				return result{}, err
			}
			if size > maxSource {
				return result{}, errors.New("Environment file exceeds 2 MiB")
			}
			args = append(args, "--env-file", snapshot.Name())
		}
		return invokeRun(ctx, workspace, executable, append(args, "--", staging)...), nil
	}
	current, err := root.ReadFile(op.Path)
	if err != nil || sha256.Sum256(current) != sha256.Sum256(source) {
		return result{}, errors.New("The file changed on disk. Reload it before saving.")
	}
	if err := root.Rename(staging, op.Path); err != nil {
		return result{}, err
	}
	return result{Revision: fmt.Sprintf("%x", sha256.Sum256([]byte(op.Source))), Output: "Saved", Fields: editableFields([]byte(op.Source))}, nil
}

func invoke(ctx context.Context, workspace, executable string, args ...string) result {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = workspace
	output, err := command.CombinedOutput()
	if err != nil && len(output) == 0 {
		output = []byte(err.Error())
	}
	return result{Output: string(output), Failed: err != nil}
}

// Expose simple scalar fields that the editor can replace without serializing YAML.
func editableFields(source []byte) []scalarField {
	var document yaml.Node
	if yaml.Unmarshal(source, &document) != nil || len(document.Content) != 1 {
		return nil
	}
	flow := valueOf(document.Content[0], "flow")
	var fields []scalarField
	for _, phase := range []string{"setup", "steps", "teardown"} {
		steps := valueOf(flow, phase)
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for i, step := range steps.Content {
			config := valueOf(step, "config")
			for _, key := range []string{"timeout", "duration"} {
				value := valueOf(config, key)
				if value == nil || value.Kind != yaml.ScalarNode || value.Style != 0 || value.Anchor != "" || value.Value == "" {
					continue
				}
				if strings.IndexFunc(value.Value, func(r rune) bool {
					return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.' || r == '-')
				}) >= 0 {
					continue
				}
				fields = append(fields, scalarField{Label: fmt.Sprintf("%s %d · %s", phase, i+1, key), Line: value.Line, Column: value.Column, Value: value.Value})
			}
		}
	}
	return fields
}

func valueOf(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func invokeRun(ctx context.Context, workspace, executable string, args ...string) result {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, args...)
	command.Dir = workspace
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	var run runner.InlineResult
	if json.Unmarshal(output, &run) == nil && run.Status != "" {
		return result{Run: &run, Failed: run.Status != "passed"}
	}
	message := strings.TrimSpace(stderr.String())
	if message == "" && err != nil {
		message = err.Error()
	}
	if message == "" {
		message = "Core returned an unreadable run result."
	}
	return result{Output: message, Failed: true}
}
