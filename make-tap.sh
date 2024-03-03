# Must run as sudo

# Create the tap interface, name it tap0.
ip tuntap add tap0 mode tap
# add a bridge to link TAP to the interface with internet access.
ip link add br0 type bridge
# link the tap0 to the bridge.
ip link set tap0 master br0
# set the tap0 up.
ip link set tap0 up
# set the bridge to use the interface with internet access.
# CAUTION: This command may disconnect the host from the internet briefly. To undo: sudo ip link set enp44s0 nomaster; sudo ip addr add <original_IP>/<subnet_mask> dev enp44s0; sudo ip link set enp44s0 up
ip link set enp44s0 master br0
# set the bridge up.
ip link set br0 up