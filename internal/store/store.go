// Package store holds the part of the configuration that can change while the
// process is running: the service list, the user list, and who may reach what.
//
// Everything else stays in config.yaml and is still read exactly once at
// startup. This file is the exception, because "add a service" and "give this
// account access to that one" are things you do from a browser, and making
// someone edit YAML, re-run -check, chown the file and restart for that is the
// whole problem this package exists to remove.
//
// The rules that govern the contents are not reimplemented here. A candidate
// state is put through config.ValidateServices and config.ValidateUsers, the
// same functions the config file goes through, so anything that persists is
// something `-check` would have accepted too.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"

	"github.com/LCBRST/webvpn/internal/config"
)

// BcryptCost is the work factor used for passwords the admin page creates. The
// same number `webvpn -hash` uses. It belongs to the hashes, which live here.
//
// Changing it does not invalidate existing hashes: bcrypt stores its own cost
// inside the hash, so an old one keeps verifying under its original factor.
const BcryptCost = 12

// version is the schema version written into the file. A future format change
// bumps it and the loader refuses what it does not understand, rather than
// silently misreading it.
const version = 1

// State is the whole persisted document.
type State struct {
	Version  int              `json:"version"`
	Users    []config.User    `json:"users"`
	Services []config.Service `json:"services"`
}

// Snapshot is a consistent read of the state: what Snapshot returns is what
// Replace takes back, so an edit is read-modify-write with a validation step in
// the middle and nothing to hand-marshal in between.
type Snapshot struct {
	Users    []config.User
	Services []config.Service

	// targets is derived from Services and deliberately unexported: it is not
	// something a caller can set, so a hand-built Snapshot cannot smuggle in a
	// target that the service rules would have refused.
	targets map[string]*url.URL
}

// Targets is the parsed address of each service, keyed by subdomain, which is
// what proxy.New needs.
func (s Snapshot) Targets() map[string]*url.URL { return s.targets }

// Store is the mutable state plus everything derived from it.
//
// The derived maps exist so that the two hot paths -- a login and a request to
// a service subdomain -- are map lookups rather than scans, and so that they
// read a consistent view: the maps and the state they came from are replaced
// together, under the same write lock.
type Store struct {
	cfg  *config.Config
	path string

	mu      sync.RWMutex
	state   State
	targets map[string]*url.URL
	hashes  map[string][]byte
	admins  map[string]bool
	// granted[user][subdomain], and only for subdomains that exist. A leftover
	// entry naming a deleted service is inert: it cannot grant anything until a
	// service with that name exists again, and the delete path cleans it up
	// anyway.
	granted map[string]map[string]bool

	// dummy is compared against when the user name is unknown, so that a
	// request for a user that does not exist costs the same as one for a user
	// that does. Without it, response time enumerates valid user names.
	//
	// Generated rather than hardcoded so it is always a hash this build's
	// bcrypt library will actually accept.
	dummy []byte
}

// New loads the state file, or seeds one from the config if it does not exist
// yet.
//
// Seeding is a migration, and it is written to preserve what the config already
// did rather than to express an opinion: every user gets every service, and if
// nobody is marked admin the first user is promoted. Both are logged. A seed
// that quietly changed what anyone could reach would be a bad way to introduce
// this feature.
func New(cfg *config.Config, logger *slog.Logger) (*Store, error) {
	dummy, err := bcrypt.GenerateFromPassword([]byte("webvpn placeholder password"), BcryptCost)
	if err != nil {
		return nil, err
	}
	s := &Store{cfg: cfg, path: cfg.State.Path, dummy: dummy}

	raw, err := os.ReadFile(s.path)
	switch {
	case err == nil:
		st, err := parse(s.path, raw)
		if err != nil {
			return nil, err
		}
		// Failing here rather than falling back to the config seed is
		// deliberate: a seed would resurrect every user who had been deleted.
		if err := s.install(st); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
		logger.Info("state loaded",
			"path", s.path, "users", len(st.Users), "services", len(st.Services),
			"note", "users: and services: in the config file are ignored")
		return s, nil

	case errors.Is(err, fs.ErrNotExist):
		st, promoted := seed(cfg)
		if err := s.install(st); err != nil {
			return nil, err
		}
		if err := writeAtomic(s.path, st); err != nil {
			return nil, fmt.Errorf("write %s: %w", s.path, err)
		}
		if promoted != "" {
			logger.Warn("no user had admin: true; promoting the first one so the admin page has an owner",
				"user", promoted)
		}
		logger.Info("state seeded from the config file",
			"path", s.path, "users", len(st.Users), "services", len(st.Services),
			"note", "from now on this file is authoritative")
		return s, nil

	default:
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
}

// parse reads a state file's bytes. Shared with New so that -check cannot
// accept a file the server would refuse.
func parse(path string, raw []byte) (State, error) {
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return State{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Version != version {
		return State{}, fmt.Errorf("%s: version %d is not supported by this build (want %d)",
			path, st.Version, version)
	}
	return st, nil
}

// Validate reports whether New would succeed against this config: it checks the
// state file if there is one, and what would be seeded if there is not.
//
// For -check, which has to be safe to run on a machine it is only meant to be
// looking at. Going through app.New instead would seed the state file as a side
// effect, turning a read-only inspection into the migration. It reads the
// existing file rather than only the seed so that a state this build cannot
// parse is caught here rather than at the next restart.
func Validate(cfg *config.Config) error {
	raw, err := os.ReadFile(cfg.State.Path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("read %s: %w", cfg.State.Path, err)
		}
		// No state yet: this is the config that would seed one. The promoted
		// name is dropped rather than logged, since nobody is being promoted.
		st, _ := seed(cfg)
		_, _, err := validate(cfg, st)
		return err
	}

	st, err := parse(cfg.State.Path, raw)
	if err != nil {
		return err
	}
	_, _, err = validate(cfg, st)
	return err
}

// seed builds the first state from the config file. See the note on New.
//
// Pure, and it returns the name of anyone it promoted rather than logging it,
// so that Validate can ask the same question without printing a promotion that
// is not happening.
func seed(cfg *config.Config) (State, string) {
	st := State{
		Version:  version,
		Users:    slices.Clone(cfg.Users),
		Services: slices.Clone(cfg.Services),
	}

	// Grant everything to everyone. Before this feature existed every login
	// reached every service, and an upgrade that silently revoked access would
	// look like the proxy had broken.
	subdomains := make([]string, 0, len(st.Services))
	for _, svc := range st.Services {
		subdomains = append(subdomains, svc.Subdomain)
	}
	for i := range st.Users {
		st.Users[i].Services = slices.Clone(subdomains)
	}

	// A config written before the admin page existed has no admin in it, and a
	// state with no admin cannot be edited back into having one -- so promote
	// one. It is one line in the log and one toggle in the UI to undo.
	promoted := ""
	if !slices.ContainsFunc(st.Users, func(u config.User) bool { return u.Admin }) {
		st.Users[0].Admin = true
		promoted = st.Users[0].Name
	}
	return st, promoted
}

// Snapshot returns a copy of the state, deep enough to edit freely: writing to
// anything it hands back cannot reach the live state. The Service structs are
// copied but their *bool fields still point at the stored ones, so a caller
// wanting to change one of those must allocate a new pointer rather than write
// through it. The admin handlers only ever overwrite those fields on their own
// copy of the struct, which is safe.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *Store) snapshotLocked() Snapshot {
	// The per-user service lists are cloned too, not just the outer slice.
	// Cloning only the outer slice would leave every User sharing its Services
	// backing array with the live state, and a handler that wrote into one --
	// `list[:0]` is the obvious way to filter in place -- would be editing the
	// running configuration without going through Replace.
	users := slices.Clone(s.state.Users)
	for i := range users {
		users[i].Services = slices.Clone(users[i].Services)
	}
	return Snapshot{
		Users:    users,
		Services: slices.Clone(s.state.Services),
		targets:  s.targets,
	}
}

// Replace validates a snapshot, persists it, and only then makes it live.
//
// Order matters. A snapshot that fails validation never reaches the disk and
// never reaches memory, so a rejected edit leaves the running server exactly as
// it was. One that validates but fails to write is equally harmless: memory is
// untouched, and the visible failure is an error message on the admin page
// rather than a change that appears to have worked and evaporates at restart.
func (s *Store) Replace(next Snapshot) error {
	st := State{Version: version, Users: next.Users, Services: next.Services}

	d, targets, err := validate(s.cfg, st)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, st); err != nil {
		return fmt.Errorf("write %s: %w", s.path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.targets = targets
	s.hashes, s.admins, s.granted = d.hashes, d.admins, d.granted
	return nil
}

// install validates a state and makes it live without writing it, for the load
// path where the bytes are already on disk.
func (s *Store) install(st State) error {
	d, targets, err := validate(s.cfg, st)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
	s.targets = targets
	s.hashes, s.admins, s.granted = d.hashes, d.admins, d.granted
	return nil
}

type derived struct {
	hashes  map[string][]byte
	admins  map[string]bool
	granted map[string]map[string]bool
}

// validate runs a candidate state through the config's own rules and builds the
// lookup maps. It touches no receiver and no disk, so ValidateSeed can call it
// on a config alone.
func validate(cfg *config.Config, st State) (*derived, map[string]*url.URL, error) {
	if err := cfg.ValidateUsers(st.Users); err != nil {
		return nil, nil, err
	}
	// ValidateServiceList rather than ValidateServices: deleting the last
	// service through the admin page is a legitimate thing to do, so an empty
	// list is a state this will hold. Every other rule is the same one the
	// config file goes through.
	targets, err := cfg.ValidateServiceList(st.Services)
	if err != nil {
		return nil, nil, err
	}

	// An admin-less state cannot be edited back into having one through the UI,
	// so it is not a state we are willing to persist. This is the invariant the
	// "cannot delete the last admin" rule on the admin page is protecting.
	hasAdmin := slices.ContainsFunc(st.Users, func(u config.User) bool { return u.Admin })
	if !hasAdmin {
		return nil, nil, errors.New("at least one user must have admin: true")
	}

	d := &derived{
		hashes:  make(map[string][]byte, len(st.Users)),
		admins:  make(map[string]bool, len(st.Users)),
		granted: make(map[string]map[string]bool, len(st.Users)),
	}
	for _, u := range st.Users {
		d.hashes[u.Name] = []byte(u.PasswordHash)
		if u.Admin {
			d.admins[u.Name] = true
		}
		granted := make(map[string]bool, len(u.Services))
		for _, sub := range u.Services {
			// Intersected with what actually exists, so a stale entry left in
			// the file can never grant access on its own.
			if _, ok := targets[sub]; ok {
				granted[sub] = true
			}
		}
		d.granted[u.Name] = granted
	}
	return d, targets, nil
}

// Allows reports whether user may reach subdomain through the tunnel.
//
// Admins reach everything. Everyone else gets exactly what was granted, and an
// unknown user gets nothing -- which is what makes deleting an account take
// effect immediately, even though the session token that account is holding is
// still cryptographically valid. Tokens carry a name, never a permission set,
// which is what lets a grant or a revocation land on the very next request.
func (s *Store) Allows(user, subdomain string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.admins[user] {
		return true
	}
	grants, ok := s.granted[user]
	if !ok {
		return false
	}
	return grants[subdomain]
}

// IsAdmin reports whether user currently holds the admin flag. Read fresh on
// every call, so demoting someone takes effect at once.
func (s *Store) IsAdmin(user string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.admins[user]
}

// Authenticate reports whether the password is correct for that user.
//
// The comparison happens outside the lock. bcrypt at cost 12 is a quarter
// second of CPU, and holding the read lock across it would stall every write
// behind a login attempt.
func (s *Store) Authenticate(name, password string) bool {
	s.mu.RLock()
	hash, known := s.hashes[name]
	if !known {
		hash = s.dummy
	}
	// Copied because the slice aliases the map's value, and the map is
	// replaced wholesale by Replace rather than mutated -- but relying on that
	// is a trap for whoever changes Replace next.
	hash = slices.Clone(hash)
	s.mu.RUnlock()

	// Always run the comparison, whatever happened above.
	matched := bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
	return known && matched
}

// writeAtomic writes the state through a temporary file in the same directory,
// so a crash or a full disk leaves either the old file or the new one and never
// a half-written one.
//
// Both fsyncs are load-bearing. Without the file's, a crash can make the rename
// durable while the contents are not, which is a rename to an empty file.
// Without the directory's, the rename itself can be lost.
func writeAtomic(path string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		if _, statErr := os.Stat(dir); statErr != nil {
			// The underlying error names a temp file nobody asked for ("open
			// /var/lib/webvpn/.state-1234.json: no such file or directory"),
			// which sends people looking at the wrong thing. The directory is
			// the missing piece, and the unit is what normally creates it.
			return fmt.Errorf("state directory %s does not exist "+
				"(deploy/webvpn.service creates it with StateDirectory=webvpn): %w", dir, err)
		}
		return err
	}
	tmp := f.Name()
	defer func() {
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	// CreateTemp already uses 0600; this is here so that the mode does not
	// depend on someone's umask being sane.
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	tmp = "" // renamed away; the deferred remove would now be a mistake

	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // best effort: not every platform lets you fsync a directory
		_ = d.Close()
	}
	return nil
}

// ServiceNames returns the subdomains in the order they appear, which is the
// order the admin page shows them in.
func (s *Snapshot) ServiceNames() []string {
	names := make([]string, 0, len(s.Services))
	for _, svc := range s.Services {
		names = append(names, svc.Subdomain)
	}
	return names
}

// IndexOfService returns the position of a service by subdomain, or -1.
func (s *Snapshot) IndexOfService(subdomain string) int {
	return slices.IndexFunc(s.Services, func(svc config.Service) bool {
		return svc.Subdomain == subdomain
	})
}

// IndexOfUser returns the position of a user by name, or -1.
func (s *Snapshot) IndexOfUser(name string) int {
	return slices.IndexFunc(s.Users, func(u config.User) bool { return u.Name == name })
}

// TrimName normalizes a submitted user name the way validation expects, so a
// stray space is a corrected save rather than a rejected one.
func TrimName(name string) string { return strings.TrimSpace(name) }
