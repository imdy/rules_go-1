// Copyright 2017 The Bazel Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// link combines the results of a compile step using "go tool link". It is invoked by the
// Go rules as an action.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/build"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
)

func run(args []string) error {
	// process the args
	if len(args) < 2 {
		return fmt.Errorf("Usage: link gotool [link args] -- <go link args>")
	}
	gotool := args[0]
	args = args[1:]
	linkargs := []string{}
	goopts := []string{}
	bctx := build.Default
	bctx.CgoEnabled = true
	for i, s := range args {
		if s == "--" {
			goopts = args[i+1:]
			break
		}
		linkargs = append(linkargs, s)
	}
	// process the flags for this link wrapper
	xdefs := multiFlag{}
	stamps := multiFlag{}
	linkstamps := multiFlag{}
	flags := flag.NewFlagSet("link", flag.ExitOnError)
	flags.Var(&xdefs, "X", "A link xdef that may need stamping.")
	flags.Var(&stamps, "stamp", "The name of a file with stamping values.")
	flags.Var(&linkstamps, "linkstamp", "A package that requires link stamping.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	goargs := []string{"tool", "link"}
	// If we were given any stamp value files, read and parse them
	stampmap := map[string]string{}
	for _, stampfile := range stamps {
		stampbuf, err := ioutil.ReadFile(stampfile)
		if err != nil {
			return fmt.Errorf("Failed reading stamp file %s: %v", stampfile, err)
		}
		scanner := bufio.NewScanner(bytes.NewReader(stampbuf))
		for scanner.Scan() {
			line := strings.SplitN(scanner.Text(), " ", 2)
			switch len(line) {
			case 0:
				// Nothing to do here
			case 1:
				// Map to the empty string
				stampmap[line[0]] = ""
			case 2:
				// Key and value
				stampmap[line[0]] = line[1]
			}
		}
	}
	// generate any additional link options we need
	for _, xdef := range xdefs {
		split := strings.SplitN(xdef, "=", 2)
		if len(split) != 2 {
			continue
		}
		name := split[0]
		key := split[1]
		if value, found := stampmap[key]; found {
			goargs = append(goargs, "-X", fmt.Sprintf("%s=%s", name, value))
		}
	}
	for _, linkstamp := range linkstamps {
		for key, value := range stampmap {
			goargs = append(goargs, "-X", fmt.Sprintf("%s.%s=%s", linkstamp, key, value))
		}
	}

	// add in the unprocess pass through options
	goargs = append(goargs, goopts...)
	cmd := exec.Command(gotool, goargs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error running linker: %v", err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
