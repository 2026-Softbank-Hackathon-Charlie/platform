//go:build !minio

package storage

// MinIOBuilt reports whether this binary has the minio driver: it is built
// with -tags minio. This one is not, so Config.Validate refuses the minio
// driver.
const MinIOBuilt = false

func newMinIO(Config, func(string) string) (Driver, error) { return nil, errMinIONotBuilt }
