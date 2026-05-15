# some basic overlays necessary for the build
final: super: {
  # nixpkgs 25.11 pinned to fe3afaa4 which ships go_1_25 at 1.25.9
  go = super.go_1_25;
  # mbedtls 3.6.5 in this nixpkgs rev unconditionally passes
  # -fzero-init-padding-bits=unions (GCC 15+) via CMAKE_C_FLAGS, but
  # stdenv uses gcc 14.3.0 which doesn't support the flag.  Strip it.
  mbedtls = super.mbedtls.overrideAttrs (old: {
    cmakeFlags = builtins.filter
      (f: f != "-DCMAKE_C_FLAGS=-fzero-init-padding-bits=unions")
      (old.cmakeFlags or [ ]);
  });
}
