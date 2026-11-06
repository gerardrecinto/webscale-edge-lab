# Alibaba Cloud: an internal Server Load Balancer in front of ECS instances that
# run shop-api, with connection draining on the listener so a drained instance
# keeps its in-flight requests.
#
# Written from the alicloud provider documentation. It has NOT been validated or
# applied: the provider registry was not reachable where this was written, so
# run `tofu init && tofu validate` before trusting it.

terraform {
  required_version = ">= 1.6"
  required_providers {
    alicloud = {
      source  = "aliyun/alicloud"
      version = "~> 1.230"
    }
  }
}

provider "alicloud" {
  region = var.region
}

variable "region" {
  type    = string
  default = "ap-southeast-1"
}

variable "instance_count" {
  type    = number
  default = 3
}

variable "instance_type" {
  type    = string
  default = "ecs.g7.large"
}

variable "image_name_regex" {
  type    = string
  default = "^ubuntu_22_04_x64"
}

variable "vpc_cidr" {
  type    = string
  default = "10.20.0.0/16"
}

variable "app_port" {
  type    = number
  default = 8080
}

data "alicloud_zones" "this" {
  available_resource_creation = "VSwitch"
}

data "alicloud_images" "ubuntu" {
  name_regex  = var.image_name_regex
  owners      = "system"
  most_recent = true
}

resource "alicloud_vpc" "this" {
  vpc_name   = "shop-api"
  cidr_block = var.vpc_cidr
}

resource "alicloud_vswitch" "this" {
  vswitch_name = "shop-api-a"
  vpc_id       = alicloud_vpc.this.id
  cidr_block   = cidrsubnet(var.vpc_cidr, 8, 1)
  zone_id      = data.alicloud_zones.this.zones[0].id
}

resource "alicloud_security_group" "app" {
  security_group_name = "shop-api"
  vpc_id              = alicloud_vpc.this.id
}

# Only the balancer's subnet may reach the app port.
resource "alicloud_security_group_rule" "app_from_slb" {
  type              = "ingress"
  ip_protocol       = "tcp"
  nic_type          = "intranet"
  policy            = "accept"
  port_range        = "${var.app_port}/${var.app_port}"
  priority          = 1
  security_group_id = alicloud_security_group.app.id
  cidr_ip           = alicloud_vswitch.this.cidr_block
}

resource "alicloud_instance" "app" {
  count                      = var.instance_count
  instance_name              = "shop-api-${count.index}"
  image_id                   = data.alicloud_images.ubuntu.images[0].id
  instance_type              = var.instance_type
  vswitch_id                 = alicloud_vswitch.this.id
  security_groups            = [alicloud_security_group.app.id]
  system_disk_category       = "cloud_essd"
  internet_max_bandwidth_out = 0 # no public address
}

resource "alicloud_slb_load_balancer" "this" {
  load_balancer_name = "shop-api"
  load_balancer_spec = "slb.s2.small"
  address_type       = "intranet"
  vswitch_id         = alicloud_vswitch.this.id
}

resource "alicloud_slb_listener" "http" {
  load_balancer_id = alicloud_slb_load_balancer.this.id
  protocol         = "http"
  frontend_port    = 80
  backend_port     = var.app_port
  bandwidth        = -1
  sticky_session   = "off"

  health_check              = "on"
  health_check_uri          = "/healthz"
  health_check_connect_port = var.app_port
  healthy_threshold         = 2
  unhealthy_threshold       = 2
  health_check_timeout      = 3
  health_check_interval     = 2

  # Keep serving requests that are already running when an instance is removed.
  connection_drain         = "on"
  connection_drain_timeout = 30
}

resource "alicloud_slb_backend_server" "app" {
  load_balancer_id = alicloud_slb_load_balancer.this.id

  dynamic "backend_servers" {
    for_each = alicloud_instance.app[*].id
    content {
      server_id = backend_servers.value
      weight    = 100
    }
  }
}

output "load_balancer_address" {
  value = alicloud_slb_load_balancer.this.address
}

output "instance_ids" {
  value = alicloud_instance.app[*].id
}
