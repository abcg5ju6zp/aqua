package registry

import "errors"

var (
	errLocalRegistryNotFound = errors.New("local registry isn't found")
	errInstallFailure        = errors.New("it failed to install some registries")
)
