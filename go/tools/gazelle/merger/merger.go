/* Copyright 2016 The Bazel Authors. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package merger provides methods for merging parsed BUILD files.
package merger

import (
	"fmt"
	"io/ioutil"
	"log"
	"sort"
	"strings"

	bzl "github.com/bazelbuild/buildtools/build"
)

const (
	gazelleIgnore = "# gazelle:ignore" // marker in a BUILD file to ignore it.
	keep          = "# keep"           // marker in srcs or deps to tell gazelle to preserve.
)

var (
	mergeableFields = map[string]bool{
		"srcs":    true,
		"deps":    true,
		"library": true,
	}
)

// MergeWithExisting merges genFile with an existing build file at
// existingFilePath and returns the merged file. If a "# gazelle:ignore" comment
// is found in the file, nil will be returned. If an error occurs, it will be
// logged, and nil will be returned.
func MergeWithExisting(genFile *bzl.File, existingFilePath string) *bzl.File {
	oldData, err := ioutil.ReadFile(existingFilePath)
	if err != nil {
		log.Print(err)
		return nil
	}
	oldFile, err := bzl.Parse(existingFilePath, oldData)
	if err != nil {
		log.Print(err)
		return nil
	}
	if shouldIgnore(oldFile) {
		return nil
	}

	oldStmt := oldFile.Stmt
	var newStmt []bzl.Expr
	for _, s := range genFile.Stmt {
		genRule, ok := s.(*bzl.CallExpr)
		if !ok {
			log.Panicf("got %v expected only CallExpr in %q", s, genFile.Path)
		}
		i, oldRule := match(oldFile, genRule)
		if oldRule == nil {
			newStmt = append(newStmt, genRule)
			continue
		}

		var mergedRule bzl.Expr
		if kind(oldRule) == "load" {
			mergedRule = mergeLoad(genRule, oldRule, oldFile)
		} else {
			mergedRule = mergeRule(genRule, oldRule)
		}
		oldStmt[i] = mergedRule
	}

	oldFile.Stmt = append(oldStmt, newStmt...)
	return oldFile
}

// merge combines information from gen and old and returns an updated rule.
// Both rules must be non-nil and must have the same kind and same name.
func mergeRule(gen, old *bzl.CallExpr) *bzl.CallExpr {
	genRule := bzl.Rule{Call: gen}
	oldRule := bzl.Rule{Call: old}
	merged := *old
	merged.List = nil
	mergedRule := bzl.Rule{Call: &merged}

	// Copy unnamed arguments from the old rule without merging. The only rule
	// generated with unnamed arguments is go_prefix, which we currently
	// leave in place.
	// TODO: maybe gazelle should allow the prefix to be changed.
	for _, a := range old.List {
		if b, ok := a.(*bzl.BinaryExpr); ok && b.Op == "=" {
			break
		}
		merged.List = append(merged.List, a)
	}

	// Merge attributes from the old rule. Preserve comments on old attributes.
	// Assume generated attributes have no comments.
	for _, k := range oldRule.AttrKeys() {
		oldAttr := oldRule.AttrDefn(k)
		if !mergeableFields[k] {
			merged.List = append(merged.List, oldAttr)
			continue
		}

		oldExpr := oldAttr.Y
		genExpr := genRule.Attr(k)
		mergedExpr, err := mergeExpr(genExpr, oldExpr)
		if err != nil {
			// TODO: add a verbose mode and log errors like this.
			mergedExpr = genExpr
		}
		if mergedExpr != nil {
			mergedAttr := *oldAttr
			mergedAttr.Y = mergedExpr
			merged.List = append(merged.List, &mergedAttr)
		}
	}

	// Merge attributes from genRule that we haven't processed already.
	for _, k := range genRule.AttrKeys() {
		if mergedRule.Attr(k) == nil {
			mergedRule.SetAttr(k, genRule.Attr(k))
		}
	}

	return &merged
}

// mergeExpr combines information from gen and old and returns an updated
// expression. The following kinds of expressions are recognized:
//
//   * nil
//   * strings (can only be merged with strings)
//   * lists of strings
//   * a call to select with a dict argument. The dict keys must be strings,
//     and the values must be lists of strings.
//   * a list of strings combined with a select call using +. The list must
//     be the left operand.
//
// An error is returned if the expressions can't be merged, for example
// because they are not in one of the above formats.
func mergeExpr(gen, old bzl.Expr) (bzl.Expr, error) {
	if _, ok := gen.(*bzl.StringExpr); ok {
		if shouldKeep(old) {
			return old, nil
		}
		return gen, nil
	}

	genList, genDict, err := exprListAndDict(gen)
	if err != nil {
		return nil, err
	}
	oldList, oldDict, err := exprListAndDict(old)
	if err != nil {
		return nil, err
	}

	mergedList := mergeList(genList, oldList)
	mergedDict, err := mergeDict(genDict, oldDict)
	if err != nil {
		return nil, err
	}

	var mergedSelect bzl.Expr
	if mergedDict != nil {
		mergedSelect = &bzl.CallExpr{
			X:    &bzl.LiteralExpr{Token: "select"},
			List: []bzl.Expr{mergedDict},
		}
	}

	if mergedList == nil {
		return mergedSelect, nil
	}
	if mergedSelect == nil {
		return mergedList, nil
	}
	mergedList.ForceMultiLine = true
	return &bzl.BinaryExpr{
		X:  mergedList,
		Op: "+",
		Y:  mergedSelect,
	}, nil
}

// exprListAndDict matches an expression and attempts to extract either a list
// of expressions, a call to select with a dictionary, or both.
// An error is returned if the expression could not be matched.
func exprListAndDict(expr bzl.Expr) (*bzl.ListExpr, *bzl.DictExpr, error) {
	if expr == nil {
		return nil, nil, nil
	}
	switch expr := expr.(type) {
	case *bzl.ListExpr:
		return expr, nil, nil
	case *bzl.CallExpr:
		if x, ok := expr.X.(*bzl.LiteralExpr); ok && x.Token == "select" && len(expr.List) == 1 {
			if d, ok := expr.List[0].(*bzl.DictExpr); ok {
				return nil, d, nil
			}
		}
	case *bzl.BinaryExpr:
		if expr.Op != "+" {
			return nil, nil, fmt.Errorf("expression could not be matched: unknown operator: %s", expr.Op)
		}
		l, ok := expr.X.(*bzl.ListExpr)
		if !ok {
			return nil, nil, fmt.Errorf("expression could not be matched: left operand not a list")
		}
		call, ok := expr.Y.(*bzl.CallExpr)
		if !ok || len(call.List) != 1 {
			return nil, nil, fmt.Errorf("expression could not be matched: right operand not a call with one argument")
		}
		x, ok := call.X.(*bzl.LiteralExpr)
		if !ok || x.Token != "select" {
			return nil, nil, fmt.Errorf("expression could not be matched: right operand not a call to select")
		}
		d, ok := call.List[0].(*bzl.DictExpr)
		if !ok {
			return nil, nil, fmt.Errorf("expression could not be matched: argument to right operand not a dict")
		}
		return l, d, nil
	}
	return nil, nil, fmt.Errorf("expression could not be matched")
}

func mergeList(gen, old *bzl.ListExpr) *bzl.ListExpr {
	if old == nil {
		return gen
	}
	if gen == nil {
		gen = &bzl.ListExpr{List: []bzl.Expr{}}
	}

	// Build a list of elements from the old list with "# keep" comments. We
	// must not duplicate these elements, since duplicate elements will be
	// removed when we rewrite the AST.
	var merged []bzl.Expr
	kept := make(map[string]bool)
	for _, v := range old.List {
		if shouldKeep(v) {
			merged = append(merged, v)
			if s := stringValue(v); s != "" {
				kept[s] = true
			}
		}
	}

	for _, v := range gen.List {
		if s := stringValue(v); kept[s] {
			continue
		}
		merged = append(merged, v)
	}

	if len(merged) == 0 {
		return nil
	}
	return &bzl.ListExpr{List: merged}
}

func mergeDict(gen, old *bzl.DictExpr) (*bzl.DictExpr, error) {
	if old == nil {
		return gen, nil
	}
	if gen == nil {
		gen = &bzl.DictExpr{List: []bzl.Expr{}}
	}

	var entries []*dictEntry
	entryMap := make(map[string]*dictEntry)

	for _, kv := range old.List {
		k, v, err := dictEntryKeyValue(kv)
		if err != nil {
			return nil, err
		}
		if _, ok := entryMap[k]; ok {
			return nil, fmt.Errorf("old dict contains more than one case named %q", k)
		}
		e := &dictEntry{key: k, oldValue: v}
		entries = append(entries, e)
		entryMap[k] = e
	}

	for _, kv := range gen.List {
		k, v, err := dictEntryKeyValue(kv)
		if err != nil {
			return nil, err
		}
		e, ok := entryMap[k]
		if !ok {
			e = &dictEntry{key: k}
			entries = append(entries, e)
			entryMap[k] = e
		}
		e.genValue = v
	}

	keys := make([]string, 0, len(entries))
	haveDefault := false
	for _, e := range entries {
		e.mergedValue = mergeList(e.genValue, e.oldValue)
		if e.key == "//conditions:default" {
			// Keep the default case, even if it's empty.
			haveDefault = true
			if e.mergedValue == nil {
				e.mergedValue = &bzl.ListExpr{}
			}
		} else if e.mergedValue != nil {
			keys = append(keys, e.key)
		}
	}
	if len(keys) == 0 && (!haveDefault || len(entryMap["//conditions:default"].mergedValue.List) == 0) {
		return nil, nil
	}
	sort.Strings(keys)
	// Always put the default case last.
	if haveDefault {
		keys = append(keys, "//conditions:default")
	}

	mergedEntries := make([]bzl.Expr, len(keys))
	for i, k := range keys {
		e := entryMap[k]
		mergedEntries[i] = &bzl.KeyValueExpr{
			Key:   &bzl.StringExpr{Value: e.key},
			Value: e.mergedValue,
		}
	}

	return &bzl.DictExpr{List: mergedEntries, ForceMultiLine: true}, nil
}

type dictEntry struct {
	key                             string
	oldValue, genValue, mergedValue *bzl.ListExpr
}

func dictEntryKeyValue(e bzl.Expr) (string, *bzl.ListExpr, error) {
	kv, ok := e.(*bzl.KeyValueExpr)
	if !ok {
		return "", nil, fmt.Errorf("dict entry was not a key-value pair: %#v", e)
	}
	k, ok := kv.Key.(*bzl.StringExpr)
	if !ok {
		return "", nil, fmt.Errorf("dict key was not string: %#v", kv.Key)
	}
	v, ok := kv.Value.(*bzl.ListExpr)
	if !ok {
		return "", nil, fmt.Errorf("dict value was not list: %#v", kv.Value)
	}
	return k.Value, v, nil
}

func mergeLoad(gen, old *bzl.CallExpr, oldfile *bzl.File) *bzl.CallExpr {
	vals := make(map[string]bzl.Expr)
	for _, v := range gen.List[1:] {
		vals[stringValue(v)] = v
	}
	for _, v := range old.List[1:] {
		rule := stringValue(v)
		if _, ok := vals[rule]; !ok && ruleUsed(rule, oldfile) {
			vals[rule] = v
		}
	}
	keys := make([]string, 0, len(vals))
	for k := range vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	merged := *old
	merged.List = old.List[:1]
	for _, k := range keys {
		merged.List = append(merged.List, vals[k])
	}
	return &merged
}

// shouldIgnore checks whether "gazelle:ignore" appears at the beginning of
// a comment before or after any top-level statement in the file.
func shouldIgnore(oldFile *bzl.File) bool {
	for _, s := range oldFile.Stmt {
		for _, c := range s.Comment().After {
			if strings.HasPrefix(c.Token, gazelleIgnore) {
				return true
			}
		}
		for _, c := range s.Comment().Before {
			if strings.HasPrefix(c.Token, gazelleIgnore) {
				return true
			}
		}
	}
	return false
}

// shouldKeep returns whether an expression from the original file should be
// preserved. This is true if it has a trailing comment that starts with "keep".
func shouldKeep(e bzl.Expr) bool {
	c := e.Comment()
	return len(c.Suffix) > 0 && strings.HasPrefix(c.Suffix[0].Token, keep)
}

func ruleUsed(rule string, oldfile *bzl.File) bool {
	return len(oldfile.Rules(rule)) != 0
}

// match looks for the matching CallExpr in f using X and name
// i.e. two 'go_library(name = "foo", ...)' are considered matches
// despite the values of the other fields.
// exception: if c is a 'load' statement, the match is done on the first value.
func match(f *bzl.File, c *bzl.CallExpr) (int, *bzl.CallExpr) {
	var m matcher
	if kind := kind(c); kind == "load" {
		if len(c.List) == 0 {
			return -1, nil
		}
		m = &loadMatcher{stringValue(c.List[0])}
	} else {
		m = &nameMatcher{kind, name(c)}
	}
	for i, s := range f.Stmt {
		other, ok := s.(*bzl.CallExpr)
		if !ok {
			continue
		}
		if m.match(other) {
			return i, other
		}
	}
	return -1, nil
}

type matcher interface {
	match(c *bzl.CallExpr) bool
}

type nameMatcher struct {
	kind, name string
}

func (m *nameMatcher) match(c *bzl.CallExpr) bool {
	return m.kind == kind(c) && m.name == name(c)
}

type loadMatcher struct {
	load string
}

func (m *loadMatcher) match(c *bzl.CallExpr) bool {
	return kind(c) == "load" && len(c.List) > 0 && m.load == stringValue(c.List[0])
}

func kind(c *bzl.CallExpr) string {
	return (&bzl.Rule{c}).Kind()
}

func name(c *bzl.CallExpr) string {
	return (&bzl.Rule{c}).Name()
}

func stringValue(e bzl.Expr) string {
	s, ok := e.(*bzl.StringExpr)
	if !ok {
		return ""
	}
	return s.Value
}
