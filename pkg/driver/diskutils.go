package driver

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
	kmount "k8s.io/mount-utils"
	kexec "k8s.io/utils/exec"
)

const (
	diskByIDPath = "/dev/disk/by-id"
	// TODO(nox): DEPRECATION B_SSD - remove legacy format when legacy volumes are fully phased out.
	legacyDiskSCWPrefix  = "scsi-0SCW_b_ssd_volume-"
	diskSCWPrefix        = "scsi-0SCW_sbs_volume-"
	diskLuksMapperPrefix = "scw-luks-"
	diskLuksMapperPath   = "/dev/mapper/"

	defaultFSType = "ext4"
)

type DiskUtils interface {
	// FormatAndMount tries to mount `devicePath` on `targetPath` as `fsType` with `mountOptions`
	// If it fails it will try to format `devicePath` as `fsType` first and retry.
	FormatAndMount(targetPath string, devicePath string, fsType string, mountOptions []string) error

	// Unmount unmounts the given target.
	Unmount(target string) error

	// MountToTarget tries to mount `sourcePath` on `targetPath` as `fsType` with `mountOptions`.
	MountToTarget(sourcePath, targetPath, fsType string, mountOptions []string) error

	// IsBlockDevice returns true if `path` is a block device.
	IsBlockDevice(path string) (bool, error)

	// GetDevicePath returns the path for the specified volumeID.
	GetDevicePath(volumeID string) (string, error)

	// IsMounted returns true is `devicePath` has a device mounted.
	IsMounted(targetPath string) bool

	// GetStatfs return the statfs struct for the given path.
	GetStatfs(path string) (*unix.Statfs_t, error)

	// Resize resizes the given volumes, it will try to resize the LUKS device first if the passphrase is provided.
	Resize(targetPath string, devicePath, passphrase string) error

	// IsEncrypted returns true if the device with the given path is encrypted with LUKS.
	IsEncrypted(devicePath string) (bool, error)

	// EncryptAndOpenDevice encrypts the volume with the given ID with the given passphrase and opens it
	// If the device is already encrypted (LUKS header present), it will only open the device.
	EncryptAndOpenDevice(volumeID string, passphrase string) (string, error)

	// CloseDevice closes the encrypted device with the given ID.
	CloseDevice(volumeID string) error

	// GetMappedDevicePath returns the path on where the encrypted device with the given ID is mapped.
	GetMappedDevicePath(volumeID string) (string, error)

	// CheckAndRepairFilesystem checks if the device is accessible and repairs dirty filesystems.
	CheckAndRepairFilesystem(devicePath string, fsType string) error
}

type diskUtils struct {
	kMounter *kmount.SafeFormatAndMount
	kResizer *kmount.ResizeFs
}

func newDiskUtils() *diskUtils {
	return &diskUtils{
		kMounter: kmount.NewSafeFormatAndMount(kmount.New(""), kexec.New()),
		kResizer: kmount.NewResizeFs(kexec.New()),
	}
}

func devicePath(volumeID string) string {
	return path.Join(diskByIDPath, diskSCWPrefix+volumeID)
}

// legacyDevicePath returns the legacy b_ssd volume path
//
// TODO(nox): DEPRECATION B_SSD - remove legacy mode when legacy volumes are fully phased out.
func legacyDevicePath(volumeID string) string {
	return path.Join(diskByIDPath, legacyDiskSCWPrefix+volumeID)
}

// EncryptAndOpenDevice encrypts the volume with the given ID with the given passphrase and opens it
// If the device is already encrypted (LUKS header present), it will only open the device.
func (d *diskUtils) EncryptAndOpenDevice(volumeID string, passphrase string) (string, error) {
	encryptedDevicePath, err := d.GetMappedDevicePath(volumeID)
	if err != nil {
		return "", err
	}

	if encryptedDevicePath != "" {
		// device is already encrypted and open
		return encryptedDevicePath, nil
	}

	// let's check if the device is aready a luks device
	devicePath, err := d.GetDevicePath(volumeID)
	if err != nil {
		return "", fmt.Errorf("error getting device path for volume %s: %w", volumeID, err)
	}

	isLuks, err := luksIsLuks(devicePath)
	if err != nil {
		return "", fmt.Errorf("error checking if device %s is a luks device: %w", devicePath, err)
	}

	if !isLuks {
		// need to format the device
		if err = luksFormat(devicePath, passphrase); err != nil {
			return "", fmt.Errorf("error formating device %s: %w", devicePath, err)
		}
	}

	if err = luksOpen(devicePath, diskLuksMapperPrefix+volumeID, passphrase); err != nil {
		if !isLuks {
			// Fresh format failed to open - no recovery possible
			return "", fmt.Errorf("error luks opening device %s: %w", devicePath, err)
		}
		// Only attempt re-format for header/device errors, NOT wrong passphrase.
		// cryptsetup exit codes: 1 = wrong parameters, 2 = no permission (bad passphrase),
		// 3 = out of memory, 4 = wrong device, 5 = device already exists/busy.
		// Treat exit codes 1 and 2 as authentication failures.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && (exitErr.ExitCode() == 1 || exitErr.ExitCode() == 2) {
			return "", fmt.Errorf("error luks opening device %s (wrong passphrase?): %w", devicePath, err)
		}
		// Existing LUKS header but open failed with a non-auth error - possibly
		// corrupted from a previous interrupted luksFormat (e.g. OOM-killed).
		// Re-format and retry.
		klog.Warningf("luksOpen failed for %s, attempting re-format (header may be corrupted): %v", devicePath, err)
		if err = luksFormat(devicePath, passphrase); err != nil {
			return "", fmt.Errorf("error re-formatting device %s after luksOpen failure: %w", devicePath, err)
		}
		if err = luksOpen(devicePath, diskLuksMapperPrefix+volumeID, passphrase); err != nil {
			return "", fmt.Errorf("error luks opening device %s after re-format: %w", devicePath, err)
		}
	}

	return diskLuksMapperPath + diskLuksMapperPrefix + volumeID, nil
}

// CloseDevice closes the encrypted device with the given ID.
func (d *diskUtils) CloseDevice(volumeID string) error {
	encryptedDevicePath, err := d.GetMappedDevicePath(volumeID)
	if err != nil {
		return err
	}

	if encryptedDevicePath != "" {
		err = luksClose(diskLuksMapperPrefix + volumeID)
		if err != nil {
			return fmt.Errorf("error luks closing %s: %w", encryptedDevicePath, err)
		}
	}

	return nil
}

// GetMappedDevicePath returns the path on where the encrypted device with the given ID is mapped
func (d *diskUtils) GetMappedDevicePath(volumeID string) (string, error) {
	mappedPath := diskLuksMapperPath + diskLuksMapperPrefix + volumeID
	if _, err := os.Stat(mappedPath); err != nil {
		// if the mapped device does not exist on disk, it's not open
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("error checking stat on %s: %w", mappedPath, err)
	}

	statusStdout, err := luksStatus(diskLuksMapperPrefix + volumeID)
	if err != nil {
		return "", fmt.Errorf("error checking luks status on %s: %w", diskLuksMapperPrefix+volumeID, err)
	}

	statusLines := strings.Split(string(statusStdout), "\n")

	if len(statusLines) == 0 {
		return "", fmt.Errorf("luksStatus stdout have 0 lines")
	}

	// first line should look like
	// /dev/mapper/<name> is active.
	// or
	// /dev/mapper/<name> is active and is in use.
	if !strings.HasSuffix(statusLines[0], "is active.") && !strings.HasSuffix(statusLines[0], "is active and is in use.") {
		// when a device is not active, an error exit code is thrown
		// something went wrong if we reach here
		return "", fmt.Errorf("luksStatus returned ok, but device %s is not active", diskLuksMapperPrefix+volumeID)
	}

	return mappedPath, nil
}

// FormatAndMount tries to mount `devicePath` on `targetPath` as `fsType` with `mountOptions`
// If it fails it will try to format `devicePath` as `fsType` first and retry.
func (d *diskUtils) FormatAndMount(targetPath string, devicePath string, fsType string, mountOptions []string) error {
	if fsType == "" {
		fsType = defaultFSType
	}

	klog.V(4).Infof("Attempting to mount %s on %s with type %s", devicePath, targetPath, fsType)

	if err := d.kMounter.FormatAndMount(devicePath, targetPath, fsType, mountOptions); err != nil {
		return fmt.Errorf("failed to optionnaly format and mount: %w", err)
	}

	return nil
}

// Unmount unmounts the given target.
func (d *diskUtils) Unmount(target string) error {
	if err := kmount.CleanupMountPoint(target, d.kMounter, true); err != nil {
		return fmt.Errorf("failed to unmount target: %w", err)
	}

	return nil
}

// MountToTarget tries to mount `sourcePath` on `targetPath` as `fsType` with `mountOptions`.
func (d *diskUtils) MountToTarget(sourcePath, targetPath, fsType string, mountOptions []string) error {
	if fsType == "" {
		fsType = defaultFSType
	}

	if err := d.kMounter.Mount(sourcePath, targetPath, fsType, mountOptions); err != nil {
		return fmt.Errorf("failed to mount to target: %w", err)
	}

	return nil
}

func (d *diskUtils) GetDevicePath(volumeID string) (string, error) {
	devicePath := devicePath(volumeID)
	realDevicePath, err := filepath.EvalSymlinks(devicePath)
	// TODO(nox): DEPRECATION B_SSD - remove legacy fallback when legacy volumes are fully phased out.
	if err != nil && errors.Is(err, fs.ErrNotExist) {
		devicePath = legacyDevicePath(volumeID)
		realDevicePath, err = filepath.EvalSymlinks(devicePath)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get real device path: %w", err)
	}

	deviceInfo, err := os.Stat(realDevicePath)
	if err != nil {
		return "", fmt.Errorf("failed to get device info: %w", err)
	}

	deviceMode := deviceInfo.Mode()
	if os.ModeDevice != deviceMode&os.ModeDevice || os.ModeCharDevice == deviceMode&os.ModeCharDevice {
		return "", errors.New("device path does not point on a block device")
	}

	return devicePath, nil
}

func (d *diskUtils) IsMounted(targetPath string) bool {
	notMnt, err := d.kMounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		return false
	}

	return !notMnt
}

func (d *diskUtils) IsBlockDevice(path string) (bool, error) {
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, fmt.Errorf("failed to get real path: %w", err)
	}

	deviceInfo, err := os.Stat(realPath)
	if err != nil {
		return false, fmt.Errorf("failed to get device info: %w", err)
	}

	deviceMode := deviceInfo.Mode()
	if os.ModeDevice != deviceMode&os.ModeDevice || os.ModeCharDevice == deviceMode&os.ModeCharDevice {
		return false, nil
	}

	return true, nil

}

func (d *diskUtils) GetStatfs(path string) (*unix.Statfs_t, error) {
	fs := &unix.Statfs_t{}
	if err := unix.Statfs(path, fs); err != nil {
		return nil, fmt.Errorf("failed to statfs: %w", err)
	}

	return fs, nil
}

func (d *diskUtils) IsEncrypted(devicePath string) (bool, error) {
	return luksIsLuks(devicePath)
}

// CheckAndRepairFilesystem checks if the device is accessible and repairs dirty filesystems.
// It probes the device with blkid to verify it's readable, then runs e2fsck for ext*
// filesystems. XFS log replay is deferred to mount, so no fsck is run for XFS.
func (d *diskUtils) CheckAndRepairFilesystem(devicePath string, fsType string) error {
	// Probe the device with blkid to check if a filesystem exists.
	// Exit codes: 0 = filesystem found, 2 = no filesystem (blank device), other = error
	probeOut, probeErr := d.kMounter.Exec.Command("blkid", "-p", "-u", "filesystem", devicePath).CombinedOutput()
	if probeErr != nil {
		// k8s.io/utils/exec wraps *os/exec.ExitError in ExitErrorWrapper which
		// implements the kexec.ExitError interface. We must use that interface
		// (not *os/exec.ExitError) for errors.As to match.
		var exitErr kexec.ExitError
		if errors.As(probeErr, &exitErr) && exitErr.ExitStatus() == 2 {
			// Exit 2 = no filesystem found — blank device, skip fsck.
			// FormatAndMount will create the filesystem.
			klog.V(4).Infof("No existing filesystem on %s (blank device), skipping fsck", devicePath)
			return nil
		}
		// Any other error (I/O errors, device not accessible) — block device is broken.
		// Do NOT proceed to FormatAndMount — it would run mkfs and destroy data.
		return fmt.Errorf("device %s is not readable (blkid: %v, output: %s); volume may not be properly attached", devicePath, probeErr, string(probeOut))
	}
	klog.V(4).Infof("Filesystem detected on %s: %s", devicePath, strings.TrimSpace(string(probeOut)))

	var cmd string
	var args []string

	switch fsType {
	case "ext4", "ext3", "ext2":
		// e2fsck -p: auto-repair safe problems (preen mode)
		// Only runs full check if superblock dirty flag is set — fast if clean.
		cmd = "e2fsck"
		args = []string{"-p", devicePath}
	case "xfs":
		// XFS log replay happens automatically on mount, so skip fsck for XFS.
		// Running xfs_repair on a filesystem with a dirty log fails unless -L is
		// used (which zeroes the log and loses data). Let mount handle log replay.
		klog.V(4).Infof("XFS filesystem on %s, skipping fsck (log replay happens on mount)", devicePath)
		return nil
	default:
		klog.V(4).Infof("No filesystem check available for fsType %s, skipping", fsType)
		return nil
	}

	klog.V(2).Infof("Running filesystem check: %s %v", cmd, args)
	out, err := d.kMounter.Exec.Command(cmd, args...).CombinedOutput()
	if err != nil {
		klog.Warningf("Filesystem check on %s returned: %v, output: %s", devicePath, err, string(out))
		// e2fsck exit code 1 = errors corrected (success). Code >= 2 = real problem.
		var exitErr kexec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitStatus() >= 2 {
			return fmt.Errorf("e2fsck found uncorrectable errors on %s (exit %d): %s", devicePath, exitErr.ExitStatus(), string(out))
		}
		// e2fsck exit 1 = corrected → success
	}

	klog.V(2).Infof("Filesystem check passed for %s", devicePath)
	return nil
}

func (d *diskUtils) Resize(targetPath string, devicePath, passphrase string) error {
	if passphrase != "" {
		klog.V(4).Infof("resizing LUKS device %s", devicePath)
		if err := luksResize(devicePath, passphrase); err != nil {
			return err
		}
	}

	klog.V(4).Infof("resizing %s", devicePath)

	needResize, err := d.kResizer.NeedResize(devicePath, targetPath)
	if err != nil {
		return fmt.Errorf("failed to check if resize is needed: %w", err)
	}

	if needResize {
		if _, err := d.kResizer.Resize(devicePath, targetPath); err != nil {
			return fmt.Errorf("failed to resize volume: %w", err)
		}
	}

	return nil
}
