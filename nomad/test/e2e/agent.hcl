# The dev agent's configuration beside -dev.
plugin "docker" {
  config {
    # The lane's images exist only on the runner, so none may be collected between rollouts.
    gc {
      image = false
    }
  }
}

client {
  options {
    # The cloud fingerprinters query metadata services, and a timeout there fails the agent's start.
    "fingerprint.denylist" = "env_aws,env_gce,env_azure,env_digitalocean"
  }
}
