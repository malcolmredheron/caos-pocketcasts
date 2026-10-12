{
  description = "caos client repo for Pocket Casts tools";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

    # THE PIN: which caos this repo's session installs and evaluates against.
    # Move it with `nix flake update caos`, then update the two `rev=` values in
    # `.caos-expr` and in `caos-tools/pocketcasts/.caos-expr` to match.
    caos.url = "github:Metta-AI/caos/main";
  };

  # Nothing is built from here; the flake only gives the pin somewhere to live.
  outputs = _: { };
}
