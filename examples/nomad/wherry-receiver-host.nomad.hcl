# The wherry receiver as a Nomad service job under the exec driver, from binaries installed on the client: the wherry
# binary /usr/local/libexec/wherry/<version>/wherry and the plugin directory /usr/local/libexec/wherry/<version>/plugins.
# /usr is in the exec driver's default chroot, so the client needs no chroot_env.
# The receiver's bundle is the item bundle of the Nomad variable nomad/jobs/wherry-receiver, which the job's workload
# identity reads. nomad/README.md has the commands that put it and run the job.
# Each receiver has its own bundle, so run one job, of count 1, per receiver.
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

    # The receiver's signing key is in its state directory on the allocation's data directory. The sticky disk with
    # migrate carries it to the next allocation, as Nomad's ephemeral_disk documentation describes.
    ephemeral_disk {
      sticky  = true
      migrate = true
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
      # The receiver needs 30 seconds to stop before a forced kill.
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

      # examples/nomad/config.yaml with the paths of this job and no credentials. A plugin profile that names no
      # credential takes NOMAD_TOKEN from the workload identity.
      template {
        destination = "local/config.yaml"
        data        = <<-EOT
          bundle: /secrets/receiver.bundle
          state_dir: /alloc/data/state
          plugin_dir: /usr/local/libexec/wherry/${var.version}/plugins
          metrics_addr: ":9464"
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
