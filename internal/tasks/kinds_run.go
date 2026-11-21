package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// The built kinds: each reads the properties of its task when the registry
// loads it, and its run calls the host. A refused property disables the
// task with a reason that names the property, never its value (ruling KN9).

// MaxHours bounds the age property of a kind, ten years in hours.
const MaxHours = 87600

// defaultHours is the age of a kind whose task sets none (ruling KN7).
const defaultHours = 24

// Kinds are the kinds of the built types of the table, on the host of ref.
func Kinds(ref *HostRef) map[string]Kind {
	return map[string]Kind{
		TypeBackup:          backupKind(ref),
		TypeMavenMetadata:   mavenKind(ref),
		TypeRescan:          rescanKind(ref),
		TypeUploadPurge:     purgeKind(ref),
		TypeWorkFiles:       workFilesKind(ref),
		TypeCompact:         compactKind(ref),
		TypeCleanup:         cleanupKind(ref),
		TypeRemoveSnapshots: snapshotsKind(ref),
	}
}

// errStopped is the failure of a run that ends because it was stopped.
var errStopped = errors.New("stopped")

// withHost is the run of a kind: it fails while ref holds no host, a run
// that ends after a stop says stopped, and a failure of the host shows its
// fixed text, any other error a fixed text of its own (ruling KN13).
func withHost(ref *HostRef, run func(ctx context.Context, h Host) (string, error)) Func {
	return func(ctx context.Context) (string, error) {
		h, err := ref.Host()
		if err != nil {
			return "", err
		}
		message, err := run(ctx, h)
		var f *Failure
		switch {
		case err == nil:
			return message, nil
		case ctx.Err() != nil:
			return "", errStopped
		case errors.As(err, &f):
			return "", f
		}
		slog.Warn("a task run failed: "+err.Error(), "logger", loggerName)
		return "", errRunFailed
	}
}

// errRunFailed is the message of a run whose host failed without a
// Failure (ruling KN13); the log of the server holds the cause.
var errRunFailed = errors.New("the run failed; the log of the server names the cause")

// PropertyError is the refusal of a property of a task of a built kind, the
// same at load and at save (ruling KN16). Reason names the property and
// never its value.
type PropertyError struct{ Field, Reason string }

func (e *PropertyError) Error() string { return e.Reason }

func refuseProperty(field, format string, args ...any) error {
	return &PropertyError{Field: field, Reason: fmt.Sprintf(format, args...)}
}

// CheckProperties is the refusal of the properties of a task of a built
// type that its kind refuses at load, a *PropertyError, and nil for a task
// the kind takes or a type that is not built (ruling KN16).
func CheckProperties(typeID string, properties map[string]string) error {
	kind, built := Kinds(&HostRef{})[typeID]
	if !built {
		return nil
	}
	_, err := kind(model.Task{TypeID: typeID, Properties: properties})
	return err
}

// required is a property that must not be empty.
func required(t model.Task, key string) (string, error) {
	value := t.Properties[key]
	if value == "" {
		return "", refuseProperty(key, "the property %s is not set", key)
	}
	return value, nil
}

// hours is the age property of a task: a whole number of hours from 1 to
// MaxHours, defaultHours when the task sets none.
func hours(t model.Task, key string) (time.Duration, error) {
	value, set := t.Properties[key]
	if !set || value == "" {
		return defaultHours * time.Hour, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 1 || n > MaxHours {
		return 0, refuseProperty(key, "the property %s needs a whole number of hours from 1 to %d", key, MaxHours)
	}
	return time.Duration(n) * time.Hour, nil
}

func backupKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		location, err := required(t, "location")
		if err != nil {
			return Job{}, err
		}
		if !filepath.IsAbs(location) {
			return Job{}, refuseProperty("location", "the property location needs an absolute directory")
		}
		// the location is a property value, which no message shows (KN9)
		return Job{Secrets: []string{location}, Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			file, err := h.BackupState(ctx, location)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("state file copied to %s, %d bytes", file.Name, file.Bytes), nil
		})}, nil
	}
}

func mavenKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		repository, err := required(t, "repositoryName")
		if err != nil {
			return Job{}, err
		}
		if repository == "*" {
			return Job{}, refuseProperty("repositoryName", "the property repositoryName needs one hosted maven2 repository, not every repository")
		}
		scope := MavenScope{GroupID: t.Properties["groupId"], ArtifactID: t.Properties["artifactId"], BaseVersion: t.Properties["baseVersion"]}
		if !mavenScope(scope) {
			return Job{}, refuseProperty("groupId",
				"the properties groupId, artifactId and baseVersion need a group for an artifact, an artifact for a version, and no part of a path")
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.RebuildMaven(ctx, repository, scope)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("maven metadata rebuilt: %d written, %d unchanged, %d removed, %d kept in a read only store",
				c.Written, c.Unchanged, c.Removed, c.Kept), nil
		})}, nil
	}
}

// mavenScope reports a scope a rebuild takes: an artifact with its group, a
// version with its artifact, and no part that is a path.
func mavenScope(s MavenScope) bool {
	if s.ArtifactID != "" && s.GroupID == "" || s.BaseVersion != "" && s.ArtifactID == "" {
		return false
	}
	var parts []string
	for _, part := range []string{s.ArtifactID, s.BaseVersion} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if s.GroupID != "" {
		parts = append(parts, strings.Split(s.GroupID, ".")...)
	}
	for _, part := range parts {
		// an empty part can only come from the group: a dot at an end or two dots
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\") {
			return false
		}
	}
	return true
}

func rescanKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		store, err := required(t, "blobStoreName")
		if err != nil {
			return Job{}, err
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.Rescan(ctx, store)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("store rescanned: %d live, %d deleted, %d copies, %d without bytes, %d unparsable",
				c.Live, c.Deleted, c.Copies, c.NoBytes, c.Unparsable), nil
		})}, nil
	}
}

func purgeKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		repository, err := required(t, "repositoryName")
		if err != nil {
			return Job{}, err
		}
		age, err := hours(t, "olderThanHours")
		if err != nil {
			return Job{}, err
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.PurgeUploads(ctx, repository, age)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("docker upload sessions: %d ended, %d kept", c.Ended, c.Kept), nil
		})}, nil
	}
}

func workFilesKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		store, err := required(t, "blobStoreName")
		if err != nil {
			return Job{}, err
		}
		age, err := hours(t, "olderThanHours")
		if err != nil {
			return Job{}, err
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.RemoveWorkFiles(ctx, store, age)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("work files: %d removed, %d kept", c.Removed, c.Kept), nil
		})}, nil
	}
}

// The bounds and defaults of the deleting kinds (rulings CL4 and CL7).
const (
	maxDays              = 3650
	defaultCompactDays   = 7
	maxRetained          = 1000
	defaultRetained      = 1
	defaultRetentionDays = 30
)

// whole is a property of whole numbers from low to high, def when the task
// sets none.
func whole(t model.Task, key string, low, high, def int) (int, error) {
	value, set := t.Properties[key]
	if !set || value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low || n > high {
		return 0, refuseProperty(key, "the property %s needs a whole number from %d to %d", key, low, high)
	}
	return n, nil
}

// dayCount is whole of days as a duration.
func dayCount(t model.Task, key string, low, def int) (time.Duration, error) {
	n, err := whole(t, key, low, maxDays, def)
	return time.Duration(n) * 24 * time.Hour, err
}

func compactKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		store := t.Properties["blobStoreName"]
		if store == "" {
			// the key of the old product, which an imported task holds
			store = t.Properties["blobstoreName"]
		}
		if store == "" {
			return Job{}, refuseProperty("blobStoreName", "the property blobStoreName is not set")
		}
		age, err := dayCount(t, "olderThanDays", 1, defaultCompactDays)
		if err != nil {
			return Job{}, err
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.Compact(ctx, store, age)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("store compacted: %d removed, %d bytes, %d kept, %d skipped", c.Removed, c.Bytes, c.Kept, c.Skipped), nil
		})}, nil
	}
}

func cleanupKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		repository := t.Properties["repositoryName"]
		if repository == "" {
			repository = "*"
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.CleanupAll(ctx, repository)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("cleanup policies: %d runs, %d refused, %d selected, %d removed, %d kept, %d skipped",
				c.Runs, c.Refused, c.Selected, c.Removed, c.Kept, c.Skipped), nil
		})}, nil
	}
}

func snapshotsKind(ref *HostRef) Kind {
	return func(t model.Task) (Job, error) {
		repository, err := required(t, "repositoryName")
		if err != nil {
			return Job{}, err
		}
		if repository == "*" {
			return Job{}, refuseProperty("repositoryName", "the property repositoryName needs one hosted maven2 repository, not every repository")
		}
		keep, err := whole(t, "minimumRetained", 0, maxRetained, defaultRetained)
		if err != nil {
			return Job{}, err
		}
		age, err := dayCount(t, "snapshotRetentionDays", 0, defaultRetentionDays)
		if err != nil {
			return Job{}, err
		}
		return Job{Run: withHost(ref, func(ctx context.Context, h Host) (string, error) {
			c, err := h.RemoveSnapshots(ctx, repository, keep, age)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("maven snapshots: %d selected, %d removed, %d kept, %d skipped", c.Selected, c.Removed, c.Kept, c.Skipped), nil
		})}, nil
	}
}
