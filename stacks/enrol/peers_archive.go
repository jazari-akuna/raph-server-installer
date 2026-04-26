// peers_archive.go — on-disk archive of rendered peer .conf files.
//
// gw0.conf does not retain the peer's WireGuard private key (only the
// public key + AllowedIPs). renderClientConf therefore emits a
// "<PRIVATE_KEY_NOT_AVAILABLE_AFTER_CREATION>" placeholder for every
// peer whose creation page has been navigated away from. To make the
// .conf re-downloadable from the UI we keep an authoritative copy on
// disk under cfg.peersArchiveDir, written at peer-create time and
// (optionally) imported by an admin via /peers/<name>/upload-conf.
//
// File layout: <peersArchiveDir>/<peer-name>.conf, mode 0600 root:root.
// Directory mode 0700 root:root. Path traversal is blocked by re-
// validating peerName against rePeerName in every helper.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func ensureArchiveDir(cfg config) error {
	if cfg.peersArchiveDir == "" {
		return errors.New("peersArchiveDir is empty")
	}
	if err := os.MkdirAll(cfg.peersArchiveDir, 0o700); err != nil {
		return err
	}
	return os.Chmod(cfg.peersArchiveDir, 0o700)
}

func archivePath(cfg config, peerName string) (string, error) {
	if !validName(peerName) {
		return "", fmt.Errorf("invalid peer name %q", peerName)
	}
	return filepath.Join(cfg.peersArchiveDir, peerName+".conf"), nil
}

func archiveExists(cfg config, peerName string) bool {
	p, err := archivePath(cfg, peerName)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func archiveRead(cfg config, peerName string) ([]byte, error) {
	p, err := archivePath(cfg, peerName)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func archiveWrite(cfg config, peerName string, conf []byte) error {
	p, err := archivePath(cfg, peerName)
	if err != nil {
		return err
	}
	if err := ensureArchiveDir(cfg); err != nil {
		return err
	}
	return atomicWrite(p, conf, 0o600)
}

func archiveDelete(cfg config, peerName string) error {
	p, err := archivePath(cfg, peerName)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
