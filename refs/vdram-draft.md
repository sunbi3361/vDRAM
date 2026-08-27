# VirtualDRAM

## 1. Overview
VirtualDRAM의 목적은 GPU thread가 DRAM 접근을 virtual address로 할 수 있도록 만들어 기존의 병목이던 translation path를 제거하는 것이다.

기본적인 메커니즘은 다음과 같다:
```
PA = VA mod sizeof(HBM)
```

VA는 PA보다 더 넓은 범위를 표현하기에 collision이 발생 할 수 있는데, VirtualDRAM에서는 SystemECC 및 On-Die ECC에 VPN tag를 embedding 하여 해결한다.

즉, 태그는 VA에서 len(PA) 부분을 걷어낸 상위 bit가 되며, 메모리 접근 시 ECC에 embedd된 tag를 통해 validation을 진행한다.
L1과 L2 Cache는 Virtually-Indexed Virtually-Tagged Cache를 가정한다.

## 2. Limitation
이 방식의 가장 큰 단점은 hash collision에 있다.

한 VPN에 할당된 PPN이 고정되기 때문에 여러 VPN이 하나의 PPN을 두고 경쟁할 수 있고, 이를 반드시 해결해줘야 한다.


