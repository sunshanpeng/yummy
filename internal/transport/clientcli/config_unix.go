//go:build unix

package clientcli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func secureInfo(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0077 != 0 {
		return errors.New("配置文件必须是当前用户所有、权限为 0600 的单链接普通文件")
	}
	return nil
}

func openSecureFile(root *os.Root, name string, flags int, create bool) (*os.File, error) {
	var before fs.FileInfo
	if !create {
		var err error
		before, err = root.Lstat(name)
		if err != nil {
			return nil, err
		}
		if err = secureInfo(before); err != nil {
			return nil, err
		}
	}
	file, err := root.OpenFile(name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil {
		err = secureInfo(info)
	}
	if err == nil && before != nil && !os.SameFile(before, info) {
		err = errors.New("配置文件在打开时发生变化")
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (s configStore) withLock(fn func(*os.Root) error) error {
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("配置目录必须是当前用户所有、权限为 0700 的非链接目录")
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return errors.New("配置目录发生变化")
	}
	// 并发首次使用时，某个调用可能刚创建目录便开始建立锁文件。
	// 先以 EXCL 原子创建，已存在时安全打开，避免创建与读取的竞态。
	lock, err := openSecureFile(root, ".lock", os.O_RDWR|os.O_CREATE|os.O_EXCL, true)
	if errors.Is(err, os.ErrExist) {
		lock, err = openSecureFile(root, ".lock", os.O_RDWR, false)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	return fn(root)
}

func atomicSecureWrite(root *os.Root, name string, data []byte) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temp := fmt.Sprintf(".tmp-%s", hex.EncodeToString(nonce[:]))
	file, err := openSecureFile(root, temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, true)
	if err != nil {
		return err
	}
	defer root.Remove(temp)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info, err := root.Lstat(name); err == nil {
		if err := secureInfo(info); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := root.Rename(temp, name); err != nil {
		return err
	}
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
