#!/usr/bin/env python3
# Split an image into RAID-5 member files (left-symmetric, like Windows LDM and Linux md),
# for testing blkmap's raid5 segment. Usage: mkraid5.py IMAGE STRIPE_BYTES OUTDIR N
import sys

image, stripe, outdir, n = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4])
data = open(image, "rb").read()
rows = len(data) // (stripe * (n - 1))
members = [bytearray(rows * stripe) for _ in range(n)]
for unit in range(rows * (n - 1)):
    row, i = divmod(unit, n - 1)
    parity = n - 1 - row % n
    member = (parity + 1 + i) % n
    chunk = data[unit * stripe:(unit + 1) * stripe]
    members[member][row * stripe:(row + 1) * stripe] = chunk
    p = members[parity]
    for k in range(stripe):
        p[row * stripe + k] ^= chunk[k]
for m, buf in enumerate(members):
    open("%s/member%d" % (outdir, m), "wb").write(buf)
