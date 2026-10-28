# The dev agent's configuration beside -dev.
plugin "docker" {
  config {
    # The lane's images exist only on the runner, so none may be collected between rollouts.
    gc {
      image = false
    }
  }
}
