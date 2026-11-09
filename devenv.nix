# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

{ pkgs, inputs, ... }:
let
  shellcheckPkgs = import inputs.nixpkgs-shellcheck { system = pkgs.stdenv.hostPlatform.system; };
  unstablePkgs = import inputs.nixpkgs-unstable { system = pkgs.stdenv.hostPlatform.system; };
  rustPkgs = import inputs.nixpkgs-rust { system = pkgs.stdenv.hostPlatform.system; };
in
{
  # Rust links with the same glibc as libclang, which bindgen loads at build time.
  # CLANG_PATH and the linker are set here because clang or its cc on PATH would shadow gcc for Go.
  env = {
    LIBCLANG_PATH = "${rustPkgs.llvmPackages_18.libclang.lib}/lib";
    CLANG_PATH = "${rustPkgs.llvmPackages_18.clang}/bin/clang";
    "CARGO_TARGET_${rustPkgs.stdenv.hostPlatform.rust.cargoEnvVarTarget}_LINKER" = "${rustPkgs.stdenv.cc}/bin/cc";
  };

  packages =
    assert pkgs.go_1_27.version == "1.27.0";
    assert pkgs.golangci-lint.version == "2.13.2";
    assert shellcheckPkgs.shellcheck.version == "0.9.0";
    assert unstablePkgs.weaver.version == "0.25.1";
    assert rustPkgs.rustc.version == "1.99.1";
    assert rustPkgs.cargo.version == "1.99.1";
    assert rustPkgs.llvmPackages_18.clang.version == "18.1.8";
    assert rustPkgs.llvmPackages_18.libclang.version == "18.1.8";
    [
      rustPkgs.cargo
      rustPkgs.rustc
      pkgs.go_1_27
      pkgs.golangci-lint
      pkgs.govulncheck
      shellcheckPkgs.shellcheck
      unstablePkgs.weaver
      unstablePkgs.gitleaks
      pkgs.gnumake
      pkgs.git
    ];
}
