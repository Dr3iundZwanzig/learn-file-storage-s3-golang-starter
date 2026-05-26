package main

import (
	"fmt"
	"mime"
	"os"
)

func (cfg apiConfig) ensureAssetsDir() error {
	if _, err := os.Stat(cfg.assetsRoot); os.IsNotExist(err) {
		return os.Mkdir(cfg.assetsRoot, 0755)
	}
	return nil
}

func getExtensionType(contentType string) (string, error) {
	extensions, err := mime.ExtensionsByType(contentType)
	if err != nil || len(extensions) == 0 {
		return "", err
	}
	ext := extensions[0]
	if ext != ".jpeg" && ext != ".png" {
		return "", fmt.Errorf("Error wrong format")
	}
	return ext, nil
}
