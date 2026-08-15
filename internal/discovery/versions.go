package discovery

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxVersionSourceBytes = 16 << 10

func detectSteamOSVersion(filePath string) string {
	if filePath == "" {
		filePath = "/etc/os-release"
	}
	contents, err := readBoundedRegular(filePath, maxVersionSourceBytes)
	if err != nil {
		return ""
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			decoded, decodeErr := strconv.Unquote(value)
			if decodeErr != nil {
				continue
			}
			value = decoded
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		}
		values[strings.TrimSpace(key)] = value
	}
	if strings.ToLower(cleanVersion(values["ID"])) != "steamos" {
		return ""
	}
	return cleanVersion(values["VERSION_ID"])
}

func detectDeckyVersion(deckyRoot string) string {
	if deckyRoot == "" {
		return ""
	}
	contents, err := readBoundedRegular(filepath.Join(deckyRoot, "services", ".loader.version"), 256)
	if err != nil {
		return ""
	}
	return cleanVersion(string(contents))
}

func readBoundedRegular(filePath string, maximum int64) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
		return nil, errors.New("version source is not a bounded regular file")
	}
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > maximum {
		return nil, errors.New("version source changed while opening")
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(contents)) != opened.Size() {
		return nil, errors.New("version source changed while reading")
	}
	return contents, nil
}

func cleanVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}
