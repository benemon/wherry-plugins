# An example only: the wherry receiver as a Nomad service job under the exec driver, from binaries installed on the
# client: the wherry binary /usr/local/libexec/wherry/<version>/wherry and the plugin directory
# /usr/local/libexec/wherry/<version>/plugins, root's and writable by no group or other user. /usr is in the exec
# driver's default chroot, so the client needs no chroot_env. CI validates this job but does not run it.
# The receiver's bundle and its local configuration, examples/nomad/config-host.yaml with the version's plugin_dir, are
# the items bundle and config of the Nomad variable nomad/jobs/wherry-receiver, which the job's workload identity reads.
# Its state directory is on a dynamic host volume like examples/nomad/wherry-receiver.volume.hcl, owned by the wherry
# user. Each receiver has its own bundle, so run one job, of count 1, per receiver. With a wherry system user and group
# on every client that can run the job, and the binaries installed and verified as nomad/README.md's Install says:
#   sed -e 's/^  uid  = 1001$/  uid  = <UID>/' -e 's/^  gid  = 0$/  gid  = <GID>/' examples/nomad/wherry-receiver.volume.hcl |
#     nomad volume create -
#   version=<version>
#   sed "s|/<version>/|/$version/|" examples/nomad/config-host.yaml >config.yaml
#   nomad var put nomad/jobs/wherry-receiver bundle=@receiver.bundle config=@config.yaml
#   nomad acl policy apply -namespace default -job wherry-receiver wherry-receiver policy.hcl   # with ACLs enabled
#   nomad job run -var version="$version" -var uid=<UID> -var gid=<GID> examples/nomad/wherry-receiver-host.nomad.hcl
# policy.hcl is the plugin's ACL policy, as for the docker job. Upgrade or roll back by installing the version on every client, recording its pin as for the docker job, then at once putting
# the item config with the version's plugin_dir and running the job again with the version. Release archives fetched
# with artifact blocks, each pinned by options.checksum from its release's verified checksums.txt, can take the place
# of the installed binaries.
variable "version" {
  type        = string
  description = "The directory under /usr/local/libexec/wherry that holds the wherry binary and its plugin directory."
}

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
        command = "/usr/local/libexec/wherry/${var.version}/wherry"
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

      # examples/nomad/config-host.yaml, its plugin_dir the version's.
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
