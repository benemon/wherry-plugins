# The wherry receiver as a Nomad service job: the image examples/nomad/Containerfile builds, under the docker driver.
# The receiver reaches the Nomad API through the task API socket and Kafka through the group's bridge network.
# The receiver's bundle is the item bundle of the Nomad variable nomad/jobs/wherry-receiver, which the job's workload
# identity reads. nomad/README.md has the commands that put it and run the job.
# Each receiver has its own bundle, so run one job, of count 1, per receiver.
variable "image" {
  type        = string
  description = "The receiver image examples/nomad/Containerfile builds."
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

    # The receiver's metrics listen on 9464 in the allocation's network namespace, mapped to a dynamic port on the
    # client.
    network {
      mode = "bridge"
      port "metrics" {
        to = 9464
      }
    }

    update {
      max_parallel      = 1
      healthy_deadline  = "2m"
      progress_deadline = "3m"
    }

    task "receiver" {
      driver = "docker"
      # The receiver's drain_timeout, 25 seconds, stays below this window.
      kill_timeout = "30s"

      config {
        image = var.image
        args  = ["receiver", "run", "--config-file", "/local/config.yaml"]
      }

      # The plugin reaches the Nomad API at the receiver configuration's address, else NOMAD_ADDR: the task API socket
      # in the task's secrets directory, which takes the workload identity's token.
      env {
        NOMAD_ADDR = "unix:///secrets/api.sock"
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

      # examples/nomad/config.yaml with the paths of this job and no credentials. A plugin profile that names no
      # credential takes NOMAD_TOKEN from the workload identity.
      template {
        destination = "local/config.yaml"
        data        = <<-EOT
          bundle: /secrets/receiver.bundle
          state_dir: /alloc/data/state
          plugin_dir: /usr/local/libexec/wherry/plugins
          metrics_addr: ":9464"
          drain_timeout: 25s
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
