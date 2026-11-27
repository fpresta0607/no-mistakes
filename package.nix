{
  lib,
  buildGoModule,
  version,
  commit ? "unknown",
  date ? "unknown",
}:

let
  buildinfo = "github.com/kunchenguid/no-mistakes/internal/buildinfo";
in
buildGoModule {
  pname = "no-mistakes";
  inherit version;

  src = lib.cleanSource ./.;
  # vendorHash covers the `go mod vendor` tree, which also changes when a new
  # import vendors another package of an already-required module, with no
  # go.mod or go.sum change. `make vendor-check` (CI's check job) compares the
  # tree's package list to this digest and fails until both are updated.
  # vendor-modules-sha256: 09b658da0170600550017bb781d5683752070e2618c31a814a2a34f975a84c23
  vendorHash = "sha256-Dk9q8mkWBR29OQsT/YXRAEJUlfkq+nxHnP79cvpSlno=";
  subPackages = [ "cmd/no-mistakes" ];

  env.CGO_ENABLED = 0;
  ldflags = [
    "-X ${buildinfo}.Version=v${version}"
    "-X ${buildinfo}.Commit=${commit}"
    "-X ${buildinfo}.Date=${date}"
    "-X ${buildinfo}.TelemetryHost=https://a.kunchenguid.com"
    "-X ${buildinfo}.TelemetryWebsiteID=f959e889-92f5-4121-8a1f-571b10861198"
  ];

  # The suite needs a real .git directory, /bin/bash, and wall-clock timing
  # the build sandbox does not provide; ci.yml runs it on three OSes instead.
  doCheck = false;

  meta = {
    description = "Local gate that reviews, tests, and fixes every push before it ships";
    homepage = "https://github.com/kunchenguid/no-mistakes";
    license = lib.licenses.mit;
    mainProgram = "no-mistakes";
  };
}
