/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package utils

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
)

// Run executes the provided command within this context
func Run(cmd *exec.Cmd) ([]byte, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		ginkgo.GinkgoWriter.Printf("chdir dir: %s\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	ginkgo.GinkgoWriter.Printf("running: %s\n", command)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("%s failed with error: (%v) %s", command, err, string(output))
	}

	return output, nil
}

// LoadImageToKindClusterWithName loads a local docker image into the kind
// cluster named by $KIND_CLUSTER (default "kind"), using the kind binary in
// $KIND (default "kind" on the PATH).
func LoadImageToKindClusterWithName(name string) error {
	cluster := "kind"
	if v, ok := os.LookupEnv("KIND_CLUSTER"); ok {
		cluster = v
	}
	kindOptions := []string{"load", "docker-image", name, "--name", cluster}
	kind := "kind"
	if v, ok := os.LookupEnv("KIND"); ok {
		kind = v
	}
	cmd := exec.Command(kind, kindOptions...)
	_, err := Run(cmd)
	return err
}

// forwardingFrom matches the line kubectl port-forward prints once it listens.
var forwardingFrom = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) ->`)

// PortForward runs `kubectl port-forward` from a free local port to port of
// target (such as "svc/name") in namespace. It returns the local port once
// kubectl listens on it, and a function that stops kubectl.
func PortForward(namespace, target string, port int) (int, func(), error) {
	cmd := exec.Command("kubectl", "port-forward", "-n", namespace, target, fmt.Sprintf(":%d", port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, nil, err
	}
	cmd.Stderr = ginkgo.GinkgoWriter
	if err := cmd.Start(); err != nil {
		return 0, nil, err
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	// ports receives the local port, or is closed when kubectl exits first.
	ports := make(chan int, 1)
	go func() {
		defer close(ports)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if m := forwardingFrom.FindStringSubmatch(scanner.Text()); m != nil {
				local, _ := strconv.Atoi(m[1])
				ports <- local
				break
			}
		}
		// Keep reading, so that kubectl never blocks on a full pipe.
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case local, ok := <-ports:
		if !ok {
			stop()
			return 0, nil, fmt.Errorf("kubectl port-forward to %s/%s exited before listening", namespace, target)
		}
		return local, stop, nil
	case <-time.After(30 * time.Second):
		stop()
		return 0, nil, fmt.Errorf("kubectl port-forward to %s/%s did not start listening within 30s", namespace, target)
	}
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, err
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}
