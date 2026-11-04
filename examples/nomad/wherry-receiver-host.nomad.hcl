# Example only: a receiver under the exec driver, from root-owned binaries installed on every eligible client.
# /usr/local/libexec/wherry/<version>/ holds wherry and plugins/. current points at the live version, and
# /usr/local/bin/wherry points at /usr/local/libexec/wherry/current/wherry. Keep directories writable only by root.
# config-host.yaml sets plugin_dir to /usr/local/libexec/wherry/current/plugins. /usr is in exec's default chroot.
# Create a dedicated wherry user and group on every eligible client. The client must run as root to set ownership.
# Each receiver has its own bundle and persistent state volume. Run one job with count 1 per receiver.
# First record the verified plugin pin and provide config-host.yaml to wherry operator receiver apply.
# Then, from the repository checkout:
#   sed -e 's/^  uid  = 1001$/  uid  = <UID>/' -e 's/^  gid  = 0$/  gid  = <GID>/' examples/nomad/wherry-receiver.volume.hcl |
#     nomad volume create -
#   nomad var put nomad/jobs/wherry-receiver bundle=@receiver.bundle config=@config.yaml
#   nomad acl policy apply -namespace default -job wherry-receiver wherry-receiver policy.hcl   # with ACLs enabled
#   nomad job run -var uid=<UID> -var gid=<GID> examples/nomad/wherry-receiver-host.nomad.hcl
# policy.hcl is the plugin ACL policy. config.yaml is the local configuration provided to receiver apply.
# Upgrade: install a new version beside the old on every client, apply its pin, repoint current, then restart the job.
# Rollback: apply the previous pin, repoint current to the previous version and restart. Preserve the state volume.
# CI validates this job but does not run it.
variable "uid" {
  type        = number
  description = "The user ID of the wherry user on the clients, which owns the rendered bundle."
}

variable "gid" {
  type        = number
  description = "The group ID of the wherry user's group on the clients."
}

job "wherry-receiver" {
  type = "service"

  group "receiver" {
    count = 1

    # The receiver's state directory, with its signing key, is on the host volume wherry-receiver, which outlives every
    # allocation: a job stop and run, or the group scaled to 0 and back, keeps it. Each receiver has its own volume.
    volume "state" {
      type            = "host"
      source          = "wherry-receiver"
      access_mode     = "single-node-single-writer"
      attachment_mode = "file-system"
      # Volume names are not unique across clients; the sticky claim binds the job to this volume's ID.
      sticky = true
    }

    update {
      max_parallel      = 1
      healthy_deadline  = "2m"
      progress_deadline = "3m"
    }

    task "receiver" {
      driver = "exec"
      # A dedicated user: the bundle holds the receiver's credentials, and nobody is shared by unrelated processes.
      user = "wherry"
      # config-host.yaml's drain_timeout, 20 seconds, stays at least 10 seconds below this window.
      kill_timeout = "30s"

      # The task shares the client's network, so the plugin reaches the Nomad API at the receiver configuration's
      # address, else NOMAD_ADDR, else the client's own 127.0.0.1:4646.
      config {
        command = "/usr/local/bin/wherry"
        args    = ["receiver", "run", "--config-file", "/local/config.yaml"]
      }

      # NOMAD_TOKEN for the plugin; with ACLs enabled, attach the plugin's ACL policy to this job.
      identity {
        env = true
      }

      # The task's user reads the bundle; Nomad sets a template's owner by numeric ID only.
      template {
        destination = "secrets/receiver.bundle"
        perms       = "0600"
        uid         = var.uid
        gid         = var.gid
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .bundle }}{{ end }}
        EOT
      }

      # The state volume at /var/lib/wherry, as in examples/nomad/config.yaml.
      volume_mount {
        volume      = "state"
        destination = "/var/lib/wherry"
      }

      # A private CA the broker's certificate chains to, the variable's item ca; empty without it.
      template {
        destination = "secrets/kafka-ca.pem"
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .ca }}{{ end }}
        EOT
      }

      # examples/nomad/config-host.yaml, its plugin_dir follows current.
      template {
        destination = "local/config.yaml"
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .config }}{{ end }}
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
