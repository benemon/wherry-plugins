# The dynamic host volume that holds the state directory of the receiver examples/nomad/wherry-receiver.nomad.hcl runs.
# nomad volume create examples/nomad/wherry-receiver.volume.hcl creates it on one client, and the job's allocations run
# on that client. Each receiver has its own volume: copy this file with another name for another receiver.
name      = "wherry-receiver"
type      = "host"
plugin_id = "mkdir"

capability {
  access_mode     = "single-node-single-writer"
  attachment_mode = "file-system"
}

# The built-in mkdir plugin creates the directory owned by the receiver's user, 1001 with group 0 in the image, and
# writable by that user alone. The receiver keeps signing.key 0600 in it.
parameters {
  mode = "0700"
  uid  = 1001
  gid  = 0
}
