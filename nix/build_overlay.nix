# some basic overlays necessary for the build
final: super: {
  # nixpkgs 25.11 pinned to fe3afaa4 which ships go_1_25 at 1.25.9
  go = super.go_1_25;
}
