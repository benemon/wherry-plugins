# The wherry receiver as a Nomad service job: the image examples/nomad/Containerfile builds, under the docker or podman
# driver.
# The receiver reaches the Nomad API through the task API socket, or the plugin's address, and Kafka through the
# group's bridge network.
# The receiver's bundle and its local configuration, examples/nomad/config.yaml, are the items bundle and config of the
# Nomad variable nomad/jobs/wherry-receiver, which the job's workload identity reads. Its state directory is on the
# dynamic host volume examples/nomad/wherry-receiver.volume.hcl specifies. nomad/README.md has the commands that create
# the volume, put the variable and run the job.
# Each receiver has its own bundle, so run one job, of count 1, per receiver.
variable "image" {
  type        = string
  description = "The receiver image examples/nomad/Containerfile builds."
}

variable "driver" {
  type        = string
  default     = "docker"
  description = "The task driver that runs the image: docker or podman."
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
      driver = var.driver
      # config.yaml's drain_timeout, 20 seconds, stays at least 10 seconds below this window.
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

      # The state volume at /var/lib/wherry, as in examples/nomad/config.yaml.
      volume_mount {
        volume      = "state"
        destination = "/var/lib/wherry"
      }

      # A private CA the broker's certificate chains to, which config.yaml's ca_file names; empty without the item ca.
      template {
        destination = "secrets/kafka-ca.pem"
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .ca }}{{ end }}
        EOT
      }

      # The Nomad API's CA, for the plugin's ca_file when it reaches the API at an HTTPS address; empty without the item
      # nomad_ca.
      template {
        destination = "secrets/nomad-ca.pem"
        data        = <<-EOT
          {{ with nomadVar "nomad/jobs/wherry-receiver" }}{{ .nomad_ca }}{{ end }}
        EOT
      }

      # examples/nomad/config.yaml.
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
