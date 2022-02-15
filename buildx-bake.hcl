variable "ENABLE_GIT_COMMAND" {
    default= "true"
}
variable "VERSION" {
    default= "workdir-dev"
}

variable "OUTPUT" {
    default= "image"
}

variable "IMAGE_REGISTRY" {
    default= "local"
}

variable "IMAGE_TAG" {
    default= "latest"
}

group "default" {
    targets = ["ccm-amd64","ccm-arm64","ccm-arm","cnm-linux-amd64","cnm-linux-arm64","cnm-linux-arm","cnm-windows-1809","cnm-windows-2004","cnm-windows-20H2","cnm-windows-ltsc2022"]
}

target "ccm-base" {
    args = {
        ENABLE_GIT_COMMAND = "$ENABLE_GIT_COMMAND"
        VERSION = "$VERSION"
    }
    output = ["type=${OUTPUT}"]
    dockerfile = "Dockerfile"
}
target "ccm-amd64" {
    inherits= ["ccm-base"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-controller-manager:${IMAGE_TAG}"
    ]
    platforms = ["linux/amd64"]
}

target "ccm-arm" {
    inherits= ["ccm-base"]
    platforms = ["linux/arm"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-controller-manager-arm:${IMAGE_TAG}"
    ]
}

target "ccm-arm64" {
    inherits= ["ccm-base"]
    platforms = ["linux/arm64"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-controller-manager-arm64:${IMAGE_TAG}"
    ]
}

target "cnm-base" {
    args = {
        ENABLE_GIT_COMMAND = "$ENABLE_GIT_COMMAND"
        VERSION = "$VERSION"
    }
    output = ["type=${OUTPUT}"]
}

target "cnm-linux" {
    inherits= ["cnm-base"]
    dockerfile = "cloud-node-manager.Dockerfile"
    platforms = ["linux/amd64","linux/arm64","linux/arm"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}"
    ]
}

target "cnm-linux-amd64" {
    inherits= ["cnm-base","cnm-linux"]
    platforms = ["linux/amd64"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-linux-amd64"
    ]
}

target "cnm-linux-arm64" {
    inherits= ["cnm-base","cnm-linux"]
    platforms = ["linux/arm64"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-linux-arm64"
    ]
}

target "cnm-linux-arm" {
    inherits= ["cnm-base","cnm-linux"]
    platforms = ["linux/arm"]
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-linux-arm"
    ]
}

target "cnm-windows-base" {
    inherits= ["cnm-base"]
    args = {
        ARCH = "amd64"
    }
    dockerfile = "cloud-node-manager-windows.Dockerfile"
    platforms = ["windows/amd64"]
}

target "cnm-windows-1809" {
    inherits= ["cnm-base","cnm-windows-base"]
    args = {
        OSVERSION = "1809"
    }
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-windows-1809-amd64"
    ]
}

target "cnm-windows-2004" {
    inherits= ["cnm-base","cnm-windows-base"]
    args = {
        OSVERSION = "2004"
    }
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-windows-2004-amd64"
    ]
}
target "cnm-windows-20H2" {
    inherits= ["cnm-base","cnm-windows-base"]
    args = {
        OSVERSION = "20H2"
    }
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-windows-20H2-amd64"
    ]
}

target "cnm-windows-ltsc2022" {
    inherits= ["cnm-base","cnm-windows-base"]
    args = {
        OSVERSION = "ltsc2022"
    }
    tags = [
        "${IMAGE_REGISTRY}/azure-cloud-node-manager:${IMAGE_TAG}-windows-ltsc2022-amd64"
    ]
}