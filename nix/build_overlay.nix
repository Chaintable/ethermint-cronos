# some basic overlays necessary for the build
final: super: {
  # nixpkgs 25.11 channel HEAD (d7a713c0) ships go_1_25 at 1.25.9
  go = super.go_1_25;
}
