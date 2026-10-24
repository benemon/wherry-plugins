//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	wherry "github.com/benemon/wherry/adapter"
)

// credentialTotal adds the run's credential IDs and values as systemd counts them against maxCredentials.
func credentialTotal(run wherry.Run) int {
	total := 0
	if run.Credential != nil {
		total += len(agentCredential) + len(run.Credential)
	}
	for _, secret := range run.Credentials {
		total += len(secret.File) + len(secret.Value)
	}
	return total
}

// writeCredentials writes each of the run's credentials to a root 0600 file in the unit's root 0700 directory, from
// which LoadCredential= reads it at the unit's start.
func writeCredentials(name string, run wherry.Run) ([]loadCredential, error) {
	files := map[string][]byte{}
	if run.Credential != nil {
		files[agentCredential] = run.Credential
	}
	for _, secret := range run.Credentials {
		files[secret.File] = secret.Value
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
