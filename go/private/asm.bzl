# Copyright 2014 The Bazel Authors. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load("@io_bazel_rules_go//go/private:common.bzl", "get_go_toolchain")

def emit_go_asm_action(ctx, source, hdrs, out_obj):
  """Construct the command line for compiling Go Assembly code.
  Constructs a symlink tree to accomodate for workspace name.
  Args:
    ctx: The skylark Context.
    source: a source code artifact
    hdrs: list of .h files that may be included
    out_obj: the artifact (configured target?) that should be produced
  """
  go_toolchain = get_go_toolchain(ctx)
  includes = depset()
  includes += [f.dirname for f in hdrs]
  includes += [f.dirname for f in go_toolchain.headers.cc.transitive_headers]
  inputs = hdrs + list(go_toolchain.headers.cc.transitive_headers) + go_toolchain.tools + [source]
  asm_args = [go_toolchain.go.path, source.path, "--", "-o", out_obj.path]
  for inc in includes:
    asm_args += ["-I", inc]
  ctx.action(
      inputs = inputs,
      outputs = [out_obj],
      mnemonic = "GoAsmCompile",
      executable = go_toolchain.asm,
      arguments = asm_args,
      env = go_toolchain.env,
  )
