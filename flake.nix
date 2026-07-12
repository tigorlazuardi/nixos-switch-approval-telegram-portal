{
  description = "NixOS switch approval Telegram portal — switchd daemon + request-switch CLI";

  # Minimal inputs on purpose: this daemon is root-capable, so the supply-chain
  # surface is kept small (see HANDOVER.md). Just nixpkgs.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # The homelab host is the only target that must build.
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
    in
    {
      packages.${system} = rec {
        # buildGoModule builds every main package under ./cmd (switchd +
        # request-switch). vendorHash = null while the module is dependency-free
        # (stdlib only); set a real hash once external deps are added.
        default = pkgs.buildGoModule {
          pname = "nixos-switch-approval-telegram-portal";
          version = "0.1.0";
          src = ./.;
          vendorHash = null;
          meta = {
            description = "switchd daemon + request-switch CLI for Telegram-approved nixos-rebuild switch";
            mainProgram = "switchd";
          };
        };
        switchd = default;
      };

      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [
          go
          gopls
          gotools
        ];
      };
    };
}
