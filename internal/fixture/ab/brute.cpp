//go:build fixture_source

#include <iostream>

int main() {
    long long a, b;
    if (!(std::cin >> a >> b)) return 2;
    std::cout << a + b << '\n';
    return 0;
}
