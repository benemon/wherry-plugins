# The wherry receiver as a Nomad service job: the image examples/nomad/Containerfile builds, under the docker driver.
# The receiver's bundle is the item bundle of the Nomad variable nomad/jobs/wherry-receiver, which the job's workload
# identity reads; put it before the job runs:
#   nomad var put nomad/jobs/wherry-receiver bundle=@receiver.bundle
#   nomad job run -var image=<image> examples/nomad/wherry-receiver.nomad.hcl
# Each receiver has its own bundle, so run one job, of count 1, per receiver.
variable "image" {
  type        = string
  description = "The receiver image examples/nomad/Containerfile builds."
}

job "wherry-receiver" {
  type = "service"

  group "receiver" {
    count = 1

    # The receiver's signing key is in its state directory on the allocation's data directory; a sticky disk carries
    # it to the next allocation on the same node.
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
      driver = "docker"
      # The receiver needs 30 seconds to stop before a forced kill.
      kill_timeout = "30s"

      config {
        image = var.image
        args  = ["receiver", "run", "--config-file", "/local/config.yaml"]
        # The plugin reaches the Nomad API at NOMAD_ADDR, else the client's own 127.0.0.1:4646.
        network_mode = "host"
      }

      # NOMAD_TOKEN for the plugin; with ACLs enabled, attach the plugin's ACL policy to this job.
      identity {
        env = true
      }

      # The image's user, 1001 with group 0, reads the bundle.
      template {
        destination = "secrets/receiver.bundle"
        perms       = "0600"
        uid         = 1001
        gid         = 0
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .bundle }}{{ end }}
        EOT
      }

      # examples/nomad/config.yaml with the paths of this job.
      template {
        destination = "local/config.yaml"
        data        = <<-EOT
          bundle: /secrets/receiver.bundle
          state_dir: /alloc/data/state
          plugin_dir: /usr/local/libexec/wherry/plugins
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
