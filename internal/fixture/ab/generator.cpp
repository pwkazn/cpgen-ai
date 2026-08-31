//go:build fixture_source

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <string>

static std::uint64_t next_value(std::uint64_t& state) {
    state += 0x9e3779b97f4a7c15ULL;
    std::uint64_t value = state;
    value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9ULL;
    value = (value ^ (value >> 27)) * 0x94d049bb133111ebULL;
    return value ^ (value >> 31);
}

static long long bounded(std::uint64_t& state) {
    return static_cast<long long>(next_value(state) % 2000000001ULL) - 1000000000LL;
}

int main(int argc, char** argv) {
    if (argc != 2) return 3;
    std::string argument(argv[1]);
    const std::string prefix = "--seed=";
    if (argument.rfind(prefix, 0) != 0 || argument.size() == prefix.size()) return 3;
    char* end = nullptr;
    std::uint64_t state = std::strtoull(argument.c_str() + prefix.size(), &end, 10);
    if (end == nullptr || *end != '\0') return 3;

    std::cout << "-1000000000 -1000000000\n";
    std::cout << "1000000000 1000000000\n";
    std::cout << "-1000000000 1000000000\n";
    std::cout << "0 0\n";
    for (int index = 0; index < 2; ++index) {
        std::cout << bounded(state) << ' ' << bounded(state) << '\n';
    }
    return 0;
}
