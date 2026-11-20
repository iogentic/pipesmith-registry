package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/admin"
	"github.com/iogentic/pipesmith-registry/internal/model"
	"github.com/iogentic/pipesmith-registry/internal/server"
	"github.com/iogentic/pipesmith-registry/internal/state"
	"github.com/iogentic/pipesmith-registry/internal/tasks"
)

// taskKinds are the task types this server runs, by type id, on the host
// of ref.
var taskKinds = tasks.Kinds

// tasksClose bounds the close of the scheduler when serve ends; tests
// shorten it.
var tasksClose = 10 * time.Second

// closing is told what serve closes, in its order; tests watch it.
var closing = func(what string) {}

const tasksLogger = "pipesmith_registry.tasks"

// taskRegistry registers the tasks of the state file of a service of
// buildSecurity on a scheduler at the clock of the machine; Serve starts it.
// A server without security has none.
func taskRegistry(sec model.Security) (*tasks.Registry, server.TaskRuns, error) {
	s, ok := sec.(*security)
	if !ok {
		return nil, nil, nil
	}
	stored, err := s.store.Tasks()
	if err != nil {
		return nil, nil, err
	}
	host := &tasks.HostRef{} // the server sets itself once it is built
	r := tasks.NewRegistry(tasks.RealClock(), tasks.Logged(s.store, &serveTasks), tasks.RegistryOptions{Kinds: taskKinds(host), DataDir: s.dataDir,
		Secrets: taskSecrets(s.dataDir), Host: host})
	if err := r.Load(stored); err != nil {
		return nil, nil, err
	}
	disabled := 0
	for _, t := range r.List() {
		if !t.Enabled {
			disabled++
		}
	}
	slog.Info(fmt.Sprintf("tasks: %d registered, %d disabled", len(stored), disabled), "logger", tasksLogger)
	return r, s.store, nil
}

// closeTasks stops the runs of the registry and waits for their ends for
// tasksClose at most.
func closeTasks(r *tasks.Registry) {
	if r == nil {
		return
	}
	closing("tasks")
	ctx, cancel := context.WithTimeout(context.Background(), tasksClose)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		slog.Warn(fmt.Sprintf("the task scheduler did not close within %s: %v", tasksClose, err), "logger", tasksLogger)
	}
}

// taskSecrets gives the secrets of a task, which no reason and no message
// of its runs shows (ruling SB11): the values of its properties whose key
// the secret word rule marks, and the values of its secret files. A secret
// file that cannot be read gives nothing.
func taskSecrets(dataDir string) func(model.Task) []string {
	return func(t model.Task) []string {
		var secrets []string
		for key, value := range t.Properties {
			if admin.SecretKey(key) && value != "" {
				secrets = append(secrets, value)
			}
		}
		files, err := state.PropertySecretFiles(dataDir, state.SecretKindTask, t.ID)
		if err != nil {
			return secrets
		}
		for _, path := range files {
			if data, err := state.ReadSecret(path); err == nil {
				if value := strings.TrimRight(string(data), "\r\n"); value != "" {
					secrets = append(secrets, value)
				}
			}
		}
		return secrets
	}
}
