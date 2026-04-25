// users.go — Authelia users_database.yml read/write + argon2id hashing.
//
// Authelia 4.39.x file-backend schema (see DESIGN.md § 0.1):
//
//   users:
//     <name>:
//       disabled: false
//       displayname: 'Sagan'
//       password: '$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>'
//       email: 'sagan@…'
//       groups: ['admins']
//
// We use these five core fields plus YAML map order (preserved by
// yaml.v3). On write we use a tmpfile-+-rename atomic update so
// Authelia's fsnotify-based file watcher reloads cleanly with no
// half-written-file race.

package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/crypto/argon2"
	"gopkg.in/yaml.v3"
)

// Argon2id parameters MUST match the values configured in
// stacks/authelia/configuration.yml § authentication_backend.file.
// password.argon2 — otherwise Authelia silently rehashes on next
// login and overwrites our YAML, which is not the end of the world
// but breaks the audit trail (the hash digest changes).
const (
	argonIterations  = 3
	argonMemoryKiB   = 65536
	argonParallelism = 4
	argonKeyLen      = 32
	argonSaltLen     = 16
)

// User mirrors the YAML schema. yaml.v3 preserves insertion order via
// yaml.Node, so we keep the file as one when reading/writing to avoid
// reordering existing entries on edit.
type User struct {
	Disabled    bool     `yaml:"disabled"`
	DisplayName string   `yaml:"displayname"`
	Password    string   `yaml:"password"`
	Email       string   `yaml:"email"`
	Groups      []string `yaml:"groups"`
}

// UsersDB wraps a yaml.Node of the whole file so we can mutate without
// reordering. The Users map is a parsed convenience view rebuilt on
// each load.
type UsersDB struct {
	mu    sync.Mutex
	path  string
	root  yaml.Node // top-level mapping with key "users"
	Users map[string]User
}

func loadUsersDB(path string) (*UsersDB, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	db := &UsersDB{path: path, Users: map[string]User{}}
	if err := yaml.Unmarshal(b, &db.root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if db.root.Kind == 0 {
		// Empty file. Construct a fresh document with `users: {}`.
		db.root = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{{
				Kind: yaml.MappingNode,
				Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "users"},
					{Kind: yaml.MappingNode},
				},
			}},
		}
	}
	// Walk to root mapping → "users" mapping.
	usersNode := findChild(documentRoot(&db.root), "users")
	if usersNode == nil {
		return nil, errors.New("users_database.yml: missing top-level `users` mapping")
	}
	for i := 0; i+1 < len(usersNode.Content); i += 2 {
		k := usersNode.Content[i]
		v := usersNode.Content[i+1]
		var u User
		if err := v.Decode(&u); err != nil {
			return nil, fmt.Errorf("decode user %s: %w", k.Value, err)
		}
		db.Users[k.Value] = u
	}
	return db, nil
}

// listSorted returns usernames sorted alphabetically — for stable UI.
func (db *UsersDB) listSorted() []string {
	out := make([]string, 0, len(db.Users))
	for k := range db.Users {
		out = append(out, k)
	}
	// Insertion order would be nicer, but Go map iteration is randomised.
	// The on-disk YAML file order is preserved separately by the yaml.Node;
	// the UI just sorts alphabetically for stability.
	sortStrings(out)
	return out
}

// upsert sets the user record (creating or replacing). Caller must hold db.mu.
func (db *UsersDB) upsert(name string, u User) error {
	if !validUsername(name) {
		return fmt.Errorf("invalid username %q", name)
	}
	usersNode := findChild(documentRoot(&db.root), "users")
	if usersNode == nil {
		return errors.New("users_database.yml: missing top-level `users` mapping")
	}
	encoded := &yaml.Node{}
	if err := encoded.Encode(u); err != nil {
		return fmt.Errorf("encode user %s: %w", name, err)
	}
	encoded.Tag = ""
	for i := 0; i+1 < len(usersNode.Content); i += 2 {
		if usersNode.Content[i].Value == name {
			usersNode.Content[i+1] = encoded
			db.Users[name] = u
			return nil
		}
	}
	usersNode.Content = append(usersNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: name},
		encoded,
	)
	db.Users[name] = u
	return nil
}

// remove drops the named user. Caller must hold db.mu.
func (db *UsersDB) remove(name string) error {
	usersNode := findChild(documentRoot(&db.root), "users")
	if usersNode == nil {
		return errors.New("users_database.yml: missing top-level `users` mapping")
	}
	for i := 0; i+1 < len(usersNode.Content); i += 2 {
		if usersNode.Content[i].Value == name {
			usersNode.Content = append(usersNode.Content[:i], usersNode.Content[i+2:]...)
			delete(db.Users, name)
			return nil
		}
	}
	return fmt.Errorf("user %q not found", name)
}

// flush writes the YAML to disk via tmpfile+rename. The rename triggers
// the parent-directory inotify event that Authelia's fsnotify watcher
// listens for.
func (db *UsersDB) flush() error {
	out, err := yaml.Marshal(&db.root)
	if err != nil {
		return fmt.Errorf("marshal users_database.yml: %w", err)
	}
	return atomicWrite(db.path, out, 0o600)
}

// --- argon2id PHC ----------------------------------------------------------

// argon2idHash computes the PHC-encoded argon2id hash with the parameters
// configured in stacks/authelia/configuration.yml. Format:
//
//	$argon2id$v=19$m=<m>,t=<t>,p=<p>$<base64(salt)>$<base64(hash)>
//
// where base64 is RFC 4648 base64-no-padding (per the argon2 reference
// implementation and matching what `authelia crypto hash generate
// argon2 --variant argon2id` produces).
func argon2idHash(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is empty")
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt,
		argonIterations, argonMemoryKiB, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	), nil
}

// --- yaml.Node helpers -----------------------------------------------------

func documentRoot(n *yaml.Node) *yaml.Node {
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		return n.Content[0]
	}
	return n
}

func findChild(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// sortStrings — stdlib sort.Strings without pulling sort here; we already
// have it in peers.go via the sort package. This wrapper exists just so
// users.go doesn't need its own import; in practice the import is already
// in peers.go and Go links the package globally.
func sortStrings(s []string) {
	// Use insertion sort for our small lists (<32 users typical).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
