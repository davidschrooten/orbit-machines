{
  description = "orbit-machines go + nix + helm development flake";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";
  inputs.flake-utils.url = "github:numtide/flake-utils";

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system: let
      pkgs = import nixpkgs {
        inherit system;
        config.allowUnfree = true;
      };
    in {
      devShells.default = pkgs.mkShell {
        nativeBuildInputs = [ pkgs.bashInteractive ];
        buildInputs = with pkgs; [
          go
          golangci-lint
          gopls
          gotools
          kubernetes-helm
          kubectl
          zstd
          git
          jq
          gnumake
        ];
        shellHook = ''
          export CGO_ENABLED=1
          export PATH="$PWD/bin:$PATH"
        '';
      };
    });
}
