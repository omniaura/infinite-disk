//go:build !darwin

package disk

const sessionClonePlatform = false

func cloneSessionPayload(from, to string) (bool, error) { return false, nil }
