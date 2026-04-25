module enrol

go 1.23

require (
	golang.org/x/crypto v0.27.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sys v0.25.0 // indirect

// External deps:
//   - golang.org/x/crypto/argon2 — argon2id password hashing for the
//     Authelia users_database.yml. Authelia uses the same RFC 9106
//     argon2id, with parameters configured in stacks/authelia/
//     configuration.yml (m=65536, t=3, p=4, keylen=32, salt=16).
//   - gopkg.in/yaml.v3 — read/write Authelia's users_database.yml.
//     stdlib has no YAML; this is the de-facto Go YAML library.
//
// Curve25519 keypair generation is still via stdlib crypto/ecdh.
// QR codes are still produced by shelling out to `qrencode`
// (installed in the runtime image).
// TOTP is via `docker exec authelia authelia storage user totp generate`,
// no Go-side TOTP code.
