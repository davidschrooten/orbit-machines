{
  description = "Orbit Machines — Kubernetes-native remote agentic development platform";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      supportedSystems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs supportedSystems (system: f (import nixpkgs { inherit system; }));
    in
    {
      packages = forAllSystems (pkgs:
        let
          buildOrbitPackage = pname: subPackage:
            pkgs.buildGoModule {
              inherit pname;
              version = "0.1.0";
              src = self;
              subPackages = [ subPackage ];
              vendorHash = null;
              ldflags = [ "-s" "-w" ];
              doCheck = false;
            };
        in
        rec {
          orbit-dispatcher = buildOrbitPackage "orbit-dispatcher" "cmd/orbit-dispatcher";
          orbit-machined = buildOrbitPackage "orbit-machined" "cmd/orbit-machined";
          default = pkgs.buildGoModule {
            pname = "orbit-machines";
            version = "0.1.0";
            src = self;
            subPackages = [
              "cmd/orbit-dispatcher"
              "cmd/orbit-machined"
            ];
            vendorHash = null;
            ldflags = [ "-s" "-w" ];
            doCheck = false;
          };
        }
      );

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.golangci-lint
            pkgs.gnumake
            pkgs.git
            pkgs.curl
            pkgs.jq
            pkgs.ripgrep
            pkgs.kubectl
            pkgs.kubernetes-helm
            pkgs.minio-client
            pkgs.direnv
          ];

          shellHook = ''
            export GOPATH="''${GOPATH:-$HOME/go}"
            export PATH="$GOPATH/bin:$PATH"
            mkdir -p "$GOPATH/bin"
          '';
        };
      });
    };
}
