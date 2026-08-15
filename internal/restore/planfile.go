package restore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxPlanBytes = 16 << 20

func SavePlan(home, directory string, plan Plan) (string, error) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(directory) {
		return "", errors.New("plan home and directory must be absolute")
	}
	if err := ValidatePlan(plan); err != nil {
		return "", err
	}
	if err := ensureSecureDirectory(home, directory); err != nil {
		return "", fmt.Errorf("create confined plan directory: %w", err)
	}
	finalPath := filepath.Join(directory, "restore-plan-"+plan.PlanID+".json")
	parent, err := openSecureParent(home, finalPath, false)
	if err != nil {
		return "", err
	}
	defer parent.root.Close()
	contents, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return "", err
	}
	contents = append(contents, '\n')
	if len(contents) > maxPlanBytes {
		return "", errors.New("restore plan exceeds the size limit")
	}
	identifier := make([]byte, 12)
	if _, err := rand.Read(identifier); err != nil {
		return "", err
	}
	temporaryName := ".restore-plan-" + hex.EncodeToString(identifier) + ".tmp"
	temporary, err := parent.root.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	remove := true
	defer func() {
		temporary.Close()
		if remove {
			_ = parent.root.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(contents); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if _, err := parent.root.Lstat(parent.name); err == nil {
		return "", errors.New("restore plan already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := renameNoReplaceRoots(parent.root, temporaryName, parent.root, parent.name); err != nil {
		return "", err
	}
	remove = false
	return finalPath, nil
}

func LoadPlan(path string) (Plan, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Plan{}, err
	}
	if !info.Mode().IsRegular() || isLinkOrReparsePoint(info) || info.Size() > maxPlanBytes {
		return Plan{}, errors.New("restore plan must be a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Plan{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxPlanBytes+1))
	decoder.DisallowUnknownFields()
	var plan Plan
	if err := decoder.Decode(&plan); err != nil {
		return Plan{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Plan{}, errors.New("restore plan contains trailing data")
	}
	if err := ValidatePlan(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}
