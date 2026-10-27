//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/benemon/wherry/pluginsdk"
)

// credentialID is the store ID of a credential: its file, or envCredential and its variable.
func credentialID(secret pluginsdk.Secret) string {
	if secret.Env != "" {
		return envCredential + secret.Env
	}
	return secret.File
}

// credentialTotal returns the larger of the run's credential IDs and values as systemd counts them against
// maxCredentials, and the pages their files take in a store that is a tmpfs of maxCredentials, as systemd 255 mounts.
func credentialTotal(run pluginsdk.Run) int {
	total, pages, page := 0, 0, os.Getpagesize()
	add := func(id string, value []byte) {
		total += len(id) + len(value)
		pages += (len(value) + page - 1) / page * page
	}
	for _, secret := range run.Credentials {
		add(credentialID(secret), secret.Value)
	}
	return max(total, pages)
}

// writeCredentials writes each of the run's credentials to a root 0600 file in the unit's root 0700 directory, from
// which LoadCredential= reads it at the unit's start.
func writeCredentials(name string, run pluginsdk.Run) ([]loadCredential, error) {
	files := map[string][]byte{}
	for _, secret := range run.Credentials {
		files[credentialID(secret)] = secret.Value
	}
	if len(files) == 0 {
		return nil, nil
	}
	dir := filepath.Join(credentialRoot, name)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	var credentials []loadCredential
	for id, value := range files {
		if err := replace(dir, id, value); err != nil {
			return nil, err
		}
		credentials = append(credentials, loadCredential{ID: id, Path: filepath.Join(dir, id)})
	}
	return credentials, nil
}

// removeCredentials removes a unit's credential files, which PID 1 has copied into the unit's store by its start's end.
func removeCredentials(name string) error {
	return os.RemoveAll(filepath.Join(credentialRoot, name))
}

// checkCredentialRoot refuses a credential directory any user but root can enter.
func checkCredentialRoot() error {
	info, err := os.Lstat(credentialRoot)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != uint32(rootUID) {
		return errors.New(credentialRoot + " must be root 0700")
	}
	return nil
}
