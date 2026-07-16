{ lib, buildGoModule }:

buildGoModule {
  pname = "nixos-switch-approval-telegram-portal";
  version = "0.2.2";
  src = ../.;
  vendorHash = null;

  meta = {
    description = "switchd daemon + request-switch CLI for Telegram-approved nixos-rebuild switch";
    mainProgram = "switchd";
  };
}
