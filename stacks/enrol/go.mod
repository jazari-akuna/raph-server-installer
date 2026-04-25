module enrol

go 1.23

// stdlib-only — no external deps. WG keypair gen uses crypto/ecdh
// (Curve25519). QR codes are produced by shelling out to `qrencode`
// (installed in the runtime image).
