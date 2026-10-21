{
  description = "codefly redis service: nix runtime (Docker-free)";

  inputs = {
    # A nixpkgs revision, not a branch: the one shipping the redis that
    # runtime-image.json pins for the Docker runtime (8.8.0 — the nixos-26.05
    # channel head of 2026-07-30, the last before stable moved to 8.8.1; 26.05
    # is also the last branch that evaluates for x86_64-darwin), so both
    # runtimes serve one redis. Move it with the image: the real-server suite
    # fails either runtime that reports another version.
    nixpkgs.url = "github:NixOS/nixpkgs/21ea275a7c46aef9d4d6ddc962e6d562e9d94183";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f system);
    in
    {
      # devShell exposes redis-server + redis-cli so the codefly NixEnvironment
      # runs them via the materialized devShell — no system install required.
      devShells = forAllSystems (system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        {
          default = pkgs.mkShell {
            packages = [
              pkgs.redis
            ];
          };
        });
    };
}
