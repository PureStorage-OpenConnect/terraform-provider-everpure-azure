variable "subscription_id" {
  type = string
}

variable "resource_group_name" {
  type        = string
  description = "Resource group the Everpure resources are created in."
}

variable "location" {
  type        = string
  description = "Azure region of the Reservation and the virtual network."
}

variable "reservation_id" {
  type        = string
  description = "Full ARM ID of the Everpure Reservation that funds the storage pool."
}

variable "subnet_id" {
  type        = string
  description = "Full ARM ID of the subnet delegated to PureStorage.Block/storagePools."
}

variable "virtual_machine_id" {
  type        = string
  description = "Full ARM ID of the virtual machine to connect."
}
