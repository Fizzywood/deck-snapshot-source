//go:build windows

package restore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func renameNoReplaceRoots(oldRoot *os.Root, oldName string, newRoot *os.Root, newName string) error {
	if filepath.Base(oldName) != oldName || filepath.Base(newName) != newName || strings.ContainsAny(oldName+newName, `/\\`) {
		return errors.New("handle-relative rename names must be single path components")
	}
	oldDirectory, err := oldRoot.Open(".")
	if err != nil {
		return err
	}
	defer oldDirectory.Close()
	newDirectory, err := newRoot.Open(".")
	if err != nil {
		return err
	}
	defer newDirectory.Close()

	objectName, err := windows.NewNTUnicodeString(oldName)
	if err != nil {
		return err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(oldDirectory.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var source windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&source,
		windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(source)

	utf16Name, err := windows.UTF16FromString(newName)
	if err != nil {
		return err
	}
	nameBytes := (len(utf16Name) - 1) * 2
	var layout fileRenameInformation
	bufferSize := int(unsafe.Offsetof(layout.FileName)) + nameBytes
	buffer := make([]byte, bufferSize)
	information := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	information.ReplaceIfExists = 0
	information.RootDirectory = windows.Handle(newDirectory.Fd())
	information.FileNameLength = uint32(nameBytes)
	destination := unsafe.Slice(&information.FileName[0], nameBytes/2)
	copy(destination, utf16Name[:len(utf16Name)-1])
	var renameStatus windows.IO_STATUS_BLOCK
	return windows.NtSetInformationFile(source, &renameStatus, &buffer[0], uint32(bufferSize), windows.FileRenameInformation)
}
