package pipeline

import "os"

func openContent(path string) (*os.File, error) {
	return os.Open(path)
}
