{ pkgs ? import <nixpkgs> {} }:

pkgs.buildEnv {
  name = "orbit-machine-env";
  paths = with pkgs; [
    nix
    bashInteractive
    coreutils
    findutils
    gnugrep
    gnused
    gawk
    gnutar
    gzip
    xz
    zstd
    which
    procps
    less
    ncurses
    cacert
    openssl
    pkg-config
    git
    gh
    nodejs
    openssh
    htop
    curl
    wget
    jq
    ripgrep
    starship
    direnv
    nix-direnv
  ];
}
