docker run -d --name='rancher' --net='host' \
    --privileged=true \
    -e TZ="America/Los_Angeles" \
    -e HOST_OS="Unraid" \
    -v '/mnt/disk1/appdata/rancher':'/var/lib/rancher':'rw' \
    --restart=unless-stopped \
    rancher/rancher:latest

apt update \
    && apt-get --assume-yes install libvirt-clients \
    && apt-get --assume-yes install nfs-common \
    && curl -L https://raw.githubusercontent.com/steve-fraser/docker-machine-kvm/master/jailer.sh > /usr/bin/jailer.sh \
    && chmod +x /usr/bin/jailer.sh
